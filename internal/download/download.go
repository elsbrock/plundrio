package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	grab "github.com/cavaliergopher/grab/v3"
	"github.com/elsbrock/plundrio/internal/log"
)

const (
	maxDownloadRetryRounds = 5
	baseDownloadRetryDelay = 30 * time.Second
)

var errDownloadStalled = errors.New("download stalled")

// downloadWorker processes download jobs from the queue
func (m *Manager) downloadWorker() {
	for {
		select {
		case <-m.stopChan:
			// Immediate shutdown requested
			log.Info("download").Msg("Worker stopping due to shutdown request")
			return
		case job := <-m.jobs:
			if m.RemovalPending(job.TransferID) {
				m.activeFiles.Delete(job.FileID)
				m.downloadRetryAttempts.Delete(job.FileID)
				continue
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
					m.downloadRetryAttempts.Delete(job.FileID)
					// Don't call FailTransfer for cancellations
					continue
				}
				// Handle permanent failures
				log.Error("download").
					Str("file_name", job.Name).
					Err(err).
					Msg("Failed to download file")

				// Remove the file from active tracking before a possible retry.
				m.activeFiles.Delete(job.FileID)

				// The normal transfer poll skips transfers with an existing
				// coordinator context. Explicitly requeue transient local
				// failures instead of merely retaining a context forever.
				if isTransientError(err) && m.scheduleDownloadRetry(job, err) {
					continue
				}

				// Mark this file as failed in the transfer context
				m.downloadRetryAttempts.Delete(job.FileID)
				m.handleFileFailure(job.TransferID)
				continue
			}
			m.downloadRetryAttempts.Delete(job.FileID)
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
			// Undo this attempt's contribution to the transfer total. The next
			// attempt restarts its byte accounting from zero, so without this
			// the bytes it already reported would be counted twice and
			// progress could climb past 100%.
			m.rollbackAttemptBytes(state)
			if m.RemovalPending(state.TransferID) {
				return NewDownloadCancelledError(state.Name, "transfer removal pending")
			}

			// Check for cancellation first - pass it through without wrapping
			if downloadErr, ok := err.(*DownloadError); ok && downloadErr.Type == "DownloadCancelled" {
				return err
			}

			lastErr = err
			if !isTransientError(err) {
				return fmt.Errorf("permanent error on attempt %d: %w", attempt, err)
			}
			if attempt < maxRetries {
				log.Warn("download").
					Str("file_name", state.Name).
					Int("attempt", attempt).
					Err(err).
					Msg("Retrying download after error")
				time.Sleep(time.Second * time.Duration(attempt))
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("failed after %d attempts, last error: %w", maxRetries, lastErr)
}

// rollbackAttemptBytes subtracts the bytes a failed attempt reported to the
// transfer total and resets the per-file counter, so a retry starts clean.
func (m *Manager) rollbackAttemptBytes(state *DownloadState) {
	state.mu.Lock()
	contributed := state.downloaded
	state.downloaded = 0
	state.Progress = 0
	state.mu.Unlock()

	if contributed <= 0 {
		return
	}
	if transferCtx, exists := m.coordinator.GetTransferContext(state.TransferID); exists {
		transferCtx.AddDownloadedBytes(-contributed)
		log.Debug("download").
			Str("file_name", state.Name).
			Int64("transfer_id", state.TransferID).
			Int64("rolled_back", contributed).
			Msg("Rolled back bytes from failed download attempt")
	}
}

// isTransientError reports whether an error is worth retrying.
//
// Matching is by substring over the whole error chain, because callers wrap
// ("download failed: %w") and grab and net/http report most of these as plain
// errors with no sentinel to compare against. The phrases are deliberately
// spelled out rather than using bare status codes: matching "503" would also
// fire on a path like "/downloads/500 Days of Summer".
func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errDownloadStalled) {
		return true
	}

	// Cancellation is deliberate, never transient.
	var downloadErr *DownloadError
	if errors.As(err, &downloadErr) && downloadErr.Type == "DownloadCancelled" {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	// Any network timeout is retryable regardless of how it is worded.
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return true
	}

	message := strings.ToLower(err.Error())
	transientMessages := []string{
		"unexpected eof",
		"connection reset",
		"connection refused",
		"i/o timeout",
		"broken pipe",
		"timeout awaiting response headers",
		"too many requests",
		"bad gateway",
		"service unavailable",
		"gateway timeout",
	}
	for _, candidate := range transientMessages {
		if strings.Contains(message, candidate) {
			return true
		}
	}

	return false
}

func downloadRetryDelay(attempt int) (time.Duration, bool) {
	if attempt < 1 || attempt > maxDownloadRetryRounds {
		return 0, false
	}

	return baseDownloadRetryDelay * time.Duration(1<<(attempt-1)), true
}

func (m *Manager) scheduleDownloadRetry(job downloadJob, err error) bool {
	transferCtx, exists := m.coordinator.GetTransferContext(job.TransferID)
	if !exists {
		m.downloadRetryAttempts.Delete(job.FileID)
		return false
	}
	if m.RemovalPending(job.TransferID) {
		m.downloadRetryAttempts.Delete(job.FileID)
		return false
	}
	value, _ := m.downloadRetryAttempts.LoadOrStore(job.FileID, 0)
	attempt := value.(int) + 1
	retryDelay := m.downloadRetryDelay
	if retryDelay == nil {
		retryDelay = downloadRetryDelay
	}
	delay, ok := retryDelay(attempt)
	if !ok {
		log.Error("download").
			Str("file_name", job.Name).
			Int64("file_id", job.FileID).
			Int("retry_rounds", attempt-1).
			Err(err).
			Msg("Download exhausted bounded retry rounds")
		return false
	}

	m.downloadRetryAttempts.Store(job.FileID, attempt)
	log.Warn("download").
		Str("file_name", job.Name).
		Int64("file_id", job.FileID).
		Int("retry_round", attempt).
		Dur("retry_in", delay).
		Err(err).
		Msg("Scheduling failed local download for retry")

	m.workerWg.Add(1)
	go func() {
		defer m.workerWg.Done()
		timer := time.NewTimer(delay)
		defer timer.Stop()

		select {
		case <-m.stopChan:
			return
		case <-timer.C:
			// Removal can reclaim its disk marker before this timer fires.
			// Queue admission checks the same generation before claiming a file.
			m.queueDownload(job, transferCtx)
		}
	}()

	return true
}

// downloadFile downloads a file from Put.io to the target directory using grab
func (m *Manager) downloadFile(state *DownloadState) error {
	if m.RemovalPending(state.TransferID) {
		return NewDownloadCancelledError(state.Name, "transfer removal pending")
	}
	// Derive context from manager's lifecycle context
	ctx, cancel := context.WithCancelCause(m.Context())
	defer cancel(nil)

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

	// grab's default client sets no timeouts at all, so a server that accepts
	// the connection and then goes quiet would hang this worker forever.
	// Note: no http.Client.Timeout — that is a whole-request deadline and
	// would kill long but healthy downloads. Stalls are caught by the
	// progress monitor instead.
	client := grab.NewClient()
	client.HTTPClient = &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			IdleConnTimeout:       m.dlConfig.IdleConnectionTimeout,
			ResponseHeaderTimeout: m.dlConfig.DownloadHeaderTimeout,
		},
	}

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
	monitorStopped := make(chan struct{})
	progressTicker := time.NewTicker(m.dlConfig.ProgressUpdateInterval)
	defer progressTicker.Stop()

	// Initialize state
	state.mu.Lock()
	state.downloaded = 0
	state.Progress = 0
	state.LastProgress = time.Now()
	state.mu.Unlock()

	// Monitor download progress. stopMonitor blocks until the monitor has
	// exited, so no progress update can land after we settle the byte
	// accounting below (or after a caller rolls this attempt back).
	go func() {
		defer close(monitorStopped)
		m.trackDownloadProgress(ctx, state, resp, done, progressTicker, cancel)
	}()
	stopMonitor := func() {
		close(done)
		<-monitorStopped
	}
	cancellationError := func(reason string) error {
		if cause := context.Cause(ctx); errors.Is(cause, errDownloadStalled) {
			return cause
		}
		return NewDownloadCancelledError(state.Name, reason)
	}

	// Wait for completion or cancellation
	select {
	case <-resp.Done:
		stopMonitor()
		// Check for errors
		if err := resp.Err(); err != nil {
			if ctx.Err() != nil {
				return cancellationError("download stopped")
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
		stopMonitor()
		return cancellationError("context cancelled")
	}
}
