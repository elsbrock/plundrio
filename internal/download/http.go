package download

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/elsbrock/plundrio/internal/log"
)

// prepareDownloadFile sets up the temporary file and paths for download
func (m *Manager) prepareDownloadFile(state *DownloadState) (string, string, *os.File, int64, error) {
	targetPath := filepath.Join(m.cfg.TargetDir, state.Name)
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return "", "", nil, 0, fmt.Errorf("failed to create directory: %w", err)
	}

	log.Debug("download").
		Str("file_name", state.Name).
		Str("target_path", targetPath).
		Msg("Preparing download paths")

	tempPath := filepath.Join(filepath.Dir(targetPath), fmt.Sprintf("download-%d.tmp", state.FileID))
	var tempFile *os.File
	var startOffset int64

	// Try to open existing temp file
	if existingFile, err := os.OpenFile(tempPath, os.O_RDWR, 0644); err == nil {
		if info, err := existingFile.Stat(); err == nil {
			startOffset = info.Size()
			tempFile = existingFile
			log.Info("download").
				Str("file_name", state.Name).
				Int64("offset", startOffset).
				Str("temp_path", tempPath).
				Msg("Resuming download from offset")
		} else {
			existingFile.Close()
			fileErr := NewFileNotFoundError(state.FileID, tempPath)
			log.Error("download").
				Str("file_name", state.Name).
				Str("temp_path", tempPath).
				Err(fileErr).
				Msg("Error stating existing file")
		}
	}

	// Create new temp file if we couldn't use existing one
	if tempFile == nil {
		var err error
		tempFile, err = os.OpenFile(tempPath, os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			return "", "", nil, 0, fmt.Errorf("failed to create temporary file: %w", err)
		}
		log.Debug("download").
			Str("file_name", state.Name).
			Str("temp_path", tempPath).
			Msg("Created new temporary file")
	}

	return targetPath, tempPath, tempFile, startOffset, nil
}

// createDownloadRequest creates and configures the HTTP request for download
func (m *Manager) createDownloadRequest(ctx context.Context, url string, startOffset int64) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	log.Debug("download").
		Str("url", url).
		Int64("offset", startOffset).
		Msg("Creating download request")

	req.Header.Set("User-Agent", "plundrio/1.0")
	if startOffset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", startOffset))
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Connection", "keep-alive")

	return req, nil
}

// createHTTPClient creates a configured HTTP client for downloads
func (m *Manager) createHTTPClient() *http.Client {
	log.Debug("download").
		Dur("idle_timeout", m.dlConfig.IdleConnectionTimeout).
		Dur("header_timeout", m.dlConfig.DownloadHeaderTimeout).
		Msg("Creating HTTP client")

	return &http.Client{
		Timeout: 0, // No timeout for large downloads
		Transport: &http.Transport{
			DisableCompression:    true,  // Disable compression for large files
			DisableKeepAlives:     false, // Enable keep-alives
			IdleConnTimeout:       m.dlConfig.IdleConnectionTimeout,
			ResponseHeaderTimeout: m.dlConfig.DownloadHeaderTimeout,
		},
	}
}

// finalizeDownload handles the completion of a download, including file operations and logging
func (m *Manager) finalizeDownload(state *DownloadState, reader *progressReader, tempFile *os.File, tempPath, targetPath string, totalSize int64) error {
	log.Debug("download").
		Str("file_name", state.Name).
		Str("temp_path", tempPath).
		Str("target_path", targetPath).
		Msg("Finalizing download")

	// Ensure all data is written to disk
	if err := tempFile.Sync(); err != nil {
		return fmt.Errorf("failed to sync file: %w", err)
	}

	// Close the temp file before moving it (if not already closed)
	if err := tempFile.Close(); err != nil && !os.IsNotExist(err) {
		log.Debug("download").
			Str("file_name", state.Name).
			Err(err).
			Msg("Temp file already closed")
	}

	// Move temp file to target path
	if err := os.Rename(tempPath, targetPath); err != nil {
		return fmt.Errorf("failed to move file to target location: %w", err)
	}

	// Clean up tracking state and update transfer progress
	m.cleanupDownload(state.FileID, state.TransferID)

	elapsed := time.Since(reader.startTime).Seconds()
	averageSpeedMBps := (float64(totalSize) / 1024 / 1024) / elapsed

	log.Info("download").
		Str("file_name", state.Name).
		Float64("size_mb", float64(totalSize)/1024/1024).
		Float64("speed_mbps", averageSpeedMBps).
		Dur("duration", time.Since(reader.startTime)).
		Str("target_path", targetPath).
		Msg("Download completed")

	return nil
}
