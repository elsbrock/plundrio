package download

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// prepareDownloadFile sets up the temporary file and paths for download
func (m *Manager) prepareDownloadFile(state *DownloadState) (string, string, *os.File, int64, error) {
	targetPath := filepath.Join(m.cfg.TargetDir, state.Name)
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return "", "", nil, 0, fmt.Errorf("failed to create directory: %w", err)
	}

	tempPath := filepath.Join(filepath.Dir(targetPath), fmt.Sprintf("download-%d.tmp", state.FileID))
	var tempFile *os.File
	var startOffset int64

	// Try to open existing temp file
	if existingFile, err := os.OpenFile(tempPath, os.O_RDWR, 0644); err == nil {
		if info, err := existingFile.Stat(); err == nil {
			startOffset = info.Size()
			tempFile = existingFile
			log.Printf("Resuming download of %s from offset %d", state.Name, startOffset)
		} else {
			existingFile.Close()
		}
	}

	// Create new temp file if we couldn't use existing one
	if tempFile == nil {
		var err error
		tempFile, err = os.OpenFile(tempPath, os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			return "", "", nil, 0, fmt.Errorf("failed to create temporary file: %w", err)
		}
	}

	return targetPath, tempPath, tempFile, startOffset, nil
}

// createDownloadRequest creates and configures the HTTP request for download
func (m *Manager) createDownloadRequest(ctx context.Context, url string, startOffset int64) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

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
	return &http.Client{
		Timeout: 0, // No timeout for large downloads
		Transport: &http.Transport{
			DisableCompression:    true,  // Disable compression for large files
			DisableKeepAlives:    false, // Enable keep-alives
			IdleConnTimeout:      idleConnectionTimeout,
			ResponseHeaderTimeout: downloadHeaderTimeout,
		},
	}
}

// finalizeDownload handles the completion of a download, including file operations and logging
func (m *Manager) finalizeDownload(state *DownloadState, reader *progressReader, tempFile *os.File, tempPath, targetPath string, totalSize int64) error {
	// Ensure all data is written to disk
	if err := tempFile.Sync(); err != nil {
		return fmt.Errorf("failed to sync file: %w", err)
	}

	// Close the temp file before moving it
	tempFile.Close()

	// Move temp file to target path
	if err := os.Rename(tempPath, targetPath); err != nil {
		return fmt.Errorf("failed to move file to target location: %w", err)
	}

	elapsed := time.Since(reader.startTime).Seconds()
	averageSpeedMBps := (float64(totalSize) / 1024 / 1024) / elapsed
	log.Printf("Completed download of %s (%.2f MB) - Average speed: %.2f MB/s",
		state.Name, float64(totalSize)/1024/1024, averageSpeedMBps)
	return nil
}
