package download

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

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
				m.coordinator.FailTransfer(job.TransferID, err)
				m.activeFiles.Delete(job.FileID)
				continue
			}
			// Handle successful downloads
			m.handleFileCompletion(job.TransferID)
			m.activeFiles.Delete(job.FileID)
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

	// Check for network errors
	if netErr, ok := err.(net.Error); ok {
		return netErr.Temporary()
	}

	// Check for HTTP status codes that indicate temporary issues
	if httpErr, ok := err.(*HTTPError); ok {
		switch httpErr.StatusCode {
		case http.StatusTooManyRequests,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout,
			http.StatusBadGateway:
			return true
		}
	}

	return false
}

// HTTPError represents an HTTP-specific error
type HTTPError struct {
	StatusCode int
	Status     string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP error: %s", e.Status)
}

// downloadFile downloads a file from Put.io to the target directory
func (m *Manager) downloadFile(state *DownloadState) error {
	// Create a context that's cancelled when either stopChan is closed or there's an error
	ctx, cancel := context.WithCancel(context.Background())

	// Set up cancellation from stopChan
	go func() {
		select {
		case <-m.stopChan:
			cancel()
		case <-ctx.Done():
		}
	}()

	defer cancel()

	// Get download URL
	url, err := m.client.GetDownloadURL(state.FileID)
	if err != nil {
		return fmt.Errorf("failed to get download URL: %w", err)
	}

	// Set up download resources
	downloadCtx, err := m.setupDownloadResources(ctx, state, url)
	if err != nil {
		return fmt.Errorf("failed to setup download resources: %w", err)
	}

	// Start the download operation
	err = m.performDownload(downloadCtx)

	// Always clean up resources
	downloadCtx.cleanup()

	// Handle errors after cleanup
	if err != nil {
		if ctx.Err() != nil {
			return NewDownloadCancelledError(state.Name, "download stopped")
		}
		return fmt.Errorf("download operation failed: %w", err)
	}

	return nil
}

// downloadContext holds all resources needed for a download operation
type downloadContext struct {
	ctx        context.Context
	cancel     context.CancelFunc
	state      *DownloadState
	url        string
	tempFile   *os.File
	tempPath   string
	targetPath string
	reader     *progressReader
	cleanup    func()
	offset     int64
}

// setupDownloadResources prepares all resources needed for download
func (m *Manager) setupDownloadResources(ctx context.Context, state *DownloadState, url string) (*downloadContext, error) {
	downloadCtx := &downloadContext{
		state: state,
		url:   url,
	}

	// Create download context with cancellation
	downloadCtx.ctx, downloadCtx.cancel = context.WithCancel(ctx)

	// Prepare file resources
	targetPath, tempPath, tempFile, startOffset, err := m.prepareDownloadFile(state)
	if err != nil {
		return nil, err
	}

	downloadCtx.tempFile = tempFile
	downloadCtx.tempPath = tempPath
	downloadCtx.targetPath = targetPath
	downloadCtx.offset = startOffset

	// Setup cleanup function
	downloadCtx.cleanup = func() {
		downloadCtx.cancel()
		if err := tempFile.Close(); err != nil {
			log.Error("download").
				Str("file_name", state.Name).
				Err(err).
				Msg("Error closing temp file")
		}
	}

	return downloadCtx, nil
}

// performDownload handles the actual download operation
func (m *Manager) performDownload(downloadCtx *downloadContext) error {
	// Create and execute request
	req, err := m.createDownloadRequest(downloadCtx.ctx, downloadCtx.url, downloadCtx.offset)
	if err != nil {
		return err
	}

	client := m.createHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to start download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return &HTTPError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
		}
	}

	// Handle download completion
	return m.handleDownloadCompletion(downloadCtx, resp)
}

// handleDownloadCompletion manages the completion of a download
func (m *Manager) handleDownloadCompletion(downloadCtx *downloadContext, resp *http.Response) error {
	state := downloadCtx.state
	totalSize := resp.ContentLength

	if totalSize <= 0 {
		return NewInvalidContentLengthError(state.Name, totalSize)
	}

	// Initialize progress tracking
	progressTicker := time.NewTicker(m.dlConfig.ProgressUpdateInterval)
	defer progressTicker.Stop()

	// Create completion channels
	done := make(chan struct{})
	copyDone := make(chan error, 1)

	// Setup progress reader
	downloadCtx.reader = m.setupProgressTracking(state, resp.Body, totalSize)

	// Start progress monitoring
	m.monitorDownloadProgress(downloadCtx.ctx, state, downloadCtx.reader, totalSize, done, progressTicker)
	m.monitorDownloadStall(downloadCtx.ctx, state, totalSize, downloadCtx.cancel)

	// Start the copy operation
	go m.copyWithProgress(downloadCtx, resp.Body, copyDone)

	// Wait for completion or cancellation
	select {
	case err := <-copyDone:
		if err != nil {
			return fmt.Errorf("download failed: %w", err)
		}
		// For successful downloads, don't log cancellation
		downloadCtx.cleanup = func() {} // Replace cleanup with no-op for successful downloads
		return m.finalizeDownload(state, downloadCtx.reader, downloadCtx.tempFile, downloadCtx.tempPath, downloadCtx.targetPath, totalSize)
	case <-downloadCtx.ctx.Done():
		return NewDownloadCancelledError(state.Name, "context cancelled")
	}
}

// copyWithProgress copies data with progress tracking
func (m *Manager) copyWithProgress(downloadCtx *downloadContext, src io.Reader, copyDone chan<- error) {
	defer close(copyDone)
	_, err := io.Copy(downloadCtx.tempFile, downloadCtx.reader)
	copyDone <- err
}
