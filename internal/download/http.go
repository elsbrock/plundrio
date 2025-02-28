package download

import (
	"fmt"
	"os"

	grab "github.com/cavaliergopher/grab/v3"
	"github.com/elsbrock/plundrio/internal/log"
)

// checkFileCompleteness verifies if a downloaded file is complete
func (m *Manager) checkFileCompleteness(state *DownloadState, filePath string, expectedSize int64) error {
	log.Debug("download").
		Str("file_name", state.Name).
		Str("file_path", filePath).
		Int64("expected_size", expectedSize).
		Msg("Checking file completeness")

	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}

	if fileInfo.Size() != expectedSize {
		return fmt.Errorf("file size mismatch: expected %d, got %d", expectedSize, fileInfo.Size())
	}

	return nil
}

// configureGrabRequest configures a grab request with appropriate options
func (m *Manager) configureGrabRequest(req *grab.Request) {
	// Set request headers
	req.HTTPRequest.Header.Set("User-Agent", "plundrio/1.0")
	req.HTTPRequest.Header.Set("Accept", "*/*")
	req.HTTPRequest.Header.Set("Connection", "keep-alive")

	// Configure request options
	req.NoCreateDirectories = false // Allow grab to create directories
	req.SkipExisting = false        // Don't skip existing files
	req.NoResume = false            // Allow resuming downloads
}

// cleanupDownloadFile handles any necessary cleanup after a download
func (m *Manager) cleanupDownloadFile(state *DownloadState, transferID int64, fileID int64) {
	// Clean up tracking state and update transfer progress
	m.cleanupDownload(fileID, transferID)

	log.Debug("download").
		Str("file_name", state.Name).
		Int64("file_id", fileID).
		Int64("transfer_id", transferID).
		Msg("Cleaned up download file")
}
