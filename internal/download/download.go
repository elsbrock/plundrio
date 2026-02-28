package download

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	grab "github.com/cavaliergopher/grab/v3"
	"github.com/elsbrock/plundrio/internal/log"
)

// downloadWorker processes download jobs from the queue
func (m *Manager) downloadWorker() {
	for {
		select {
		case <-m.stopChan:
			// Immediate shutdown requested
			log.Info("download").Msg("Worker stopping due to shutdown request")
			return
		case job, ok := <-m.jobs:
			if !ok {
				return
			}
			state := &DownloadState{
				FileID:     job.FileID,
				Name:       job.Name,
				TransferID: job.TransferID,
				StartTime:  time.Now(),
			}
			err := m.downloadWithRetry(state)
			if err != nil {
				if downloadErr, ok := err.(*DownloadError); ok && downloadErr.Type == "DownloadCancelled" {
					log.Info("download").
						Str("file_name", job.Name).
						Msg("Download cancelled due to shutdown")
					// Just remove from active files for cancelled downloads
					m.activeFiles.Delete(job.FileID)
					// Don't call FailTransfer for cancellations
					continue
				}
				// Handle permanent failures
				log.Error("download").
					Str("file_name", job.Name).
					Err(err).
					Msg("Failed to download file")

				// Just remove the file from active files but don't fail the entire transfer
				// We'll keep the transfer context so we can retry later
				m.activeFiles.Delete(job.FileID)

				// Mark this file as failed in the transfer context
				m.handleFileFailure(job.TransferID)
				continue
			}
			// Pass both transferID and fileID to handleFileCompletion
			// The file cleanup is now handled inside handleFileCompletion
			m.handleFileCompletion(job.TransferID, job.FileID)
			// Do NOT call m.activeFiles.Delete here - now handled in handleFileCompletion
		}
	}
}

// downloadWithRetry attempts to download a file with retries on transient errors
func (m *Manager) downloadWithRetry(state *DownloadState) error {
	const maxRetries = 3
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		if err := m.downloadFile(state); err != nil {
			// Check for cancellation first - pass it through without wrapping
			if downloadErr, ok := err.(*DownloadError); ok && downloadErr.Type == "DownloadCancelled" {
				return err
			}

			lastErr = err
			if !isTransientError(err) {
				return fmt.Errorf("permanent error on attempt %d: %w", attempt, err)
			}
			log.Warn("download").
				Str("file_name", state.Name).
				Int("attempt", attempt).
				Err(err).
				Msg("Retrying download after error")
			time.Sleep(time.Second * time.Duration(attempt))
			continue
		}
		return nil
	}
	return fmt.Errorf("failed after %d attempts, last error: %w", maxRetries, lastErr)
}

// isTransientError determines if an error is potentially recoverable
func isTransientError(err error) bool {
	if err == nil {
		return false
	}

	// Check for cancellation errors - these should be passed through
	if downloadErr, ok := err.(*DownloadError); ok && downloadErr.Type == "DownloadCancelled" {
		return false
	}

	// Check for grab errors
	if err.Error() == "connection reset" ||
		err.Error() == "connection refused" ||
		err.Error() == "i/o timeout" {
		return true
	}

	// Check for specific grab HTTP errors
	if strings.Contains(err.Error(), "429") || // Too Many Requests
		strings.Contains(err.Error(), "503") || // Service Unavailable
		strings.Contains(err.Error(), "504") || // Gateway Timeout
		strings.Contains(err.Error(), "502") { // Bad Gateway
		return true
	}

	return false
}

// downloadFile downloads a file from Put.io to the target directory using grab
func (m *Manager) downloadFile(state *DownloadState) error {
	// Derive context from manager's lifecycle context
	ctx, cancel := context.WithCancel(m.Context())
	defer cancel()

	// Get download URL
	url, err := m.client.GetDownloadURL(ctx, state.FileID)
	if err != nil {
		return fmt.Errorf("failed to get download URL: %w", err)
	}

	// Prepare target path
	targetPath := filepath.Join(m.cfg.TargetDir, state.Name)
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Create grab client with our configuration
	client := grab.NewClient()

	// Create grab request
	req, err := grab.NewRequest(targetPath, url)
	if err != nil {
		return fmt.Errorf("failed to create download request: %w", err)
	}

	// Set request context for cancellation
	req = req.WithContext(ctx)

	// Set request headers
	req.HTTPRequest.Header.Set("User-Agent", "plundrio/1.0")
	req.HTTPRequest.Header.Set("Accept", "*/*")
	req.HTTPRequest.Header.Set("Connection", "keep-alive")

	// Start the download
	log.Info("download").
		Str("file_name", state.Name).
		Str("target_path", targetPath).
		Msg("Starting download with grab")

	// Execute the request
	resp := client.Do(req)

	// Set up progress tracking
	done := make(chan struct{})
	progressTicker := time.NewTicker(m.dlConfig.ProgressUpdateInterval)
	defer progressTicker.Stop()

	// Initialize state
	state.mu.Lock()
	state.downloaded = 0
	state.Progress = 0
	state.LastProgress = time.Now()
	state.mu.Unlock()

	// Monitor download progress
	go m.monitorGrabDownloadProgress(ctx, state, resp, done, progressTicker)

	// Wait for completion or cancellation
	select {
	case <-resp.Done:
		close(done)
		// Check for errors
		if err := resp.Err(); err != nil {
			if ctx.Err() != nil {
				return NewDownloadCancelledError(state.Name, "download stopped")
			}
			return fmt.Errorf("download failed: %w", err)
		}

		// Verify file completeness
		if !resp.IsComplete() {
			return fmt.Errorf("download incomplete: %s", state.Name)
		}

		// Log completion
		elapsed := time.Since(state.StartTime).Seconds()
		totalSize := resp.Size()
		averageSpeedMBps := (float64(totalSize) / 1024 / 1024) / elapsed

		// Flush any remaining bytes not yet reported by the progress ticker.
		// The ticker adds incremental deltas; this catches the gap between
		// the last tick and actual completion so we don't double-count.
		if transferCtx, exists := m.coordinator.GetTransferContext(state.TransferID); exists {
			state.mu.Lock()
			finalDelta := totalSize - state.downloaded
			state.downloaded = totalSize
			state.mu.Unlock()

			if finalDelta > 0 {
				transferCtx.AddDownloadedBytes(finalDelta)
			}

			downloadedSize, transferTotal, _, _ := transferCtx.GetProgress()
			log.Debug("download").
				Str("file_name", state.Name).
				Int64("transfer_id", state.TransferID).
				Int64("final_delta", finalDelta).
				Int64("transfer_downloaded", downloadedSize).
				Int64("transfer_total", transferTotal).
				Msg("Flushed remaining download bytes")
		}

		log.Info("download").
			Str("file_name", state.Name).
			Float64("size_mb", float64(totalSize)/1024/1024).
			Float64("speed_mbps", averageSpeedMBps).
			Dur("duration", time.Since(state.StartTime)).
			Str("target_path", targetPath).
			Msg("Download completed")

		return nil

	case <-ctx.Done():
		close(done)
		return NewDownloadCancelledError(state.Name, "context cancelled")
	}
}
