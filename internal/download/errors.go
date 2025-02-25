package download

import (
	"fmt"
	"time"
)

// DownloadError is the base error type for download-related errors
type DownloadError struct {
	Type    string
	Message string
}

// Error implements the error interface
func (e *DownloadError) Error() string {
	return fmt.Sprintf("%s: %s", e.Type, e.Message)
}

// NewDownloadCancelledError creates a new error for cancelled downloads
func NewDownloadCancelledError(filename, reason string) error {
	return &DownloadError{
		Type:    "DownloadCancelled",
		Message: fmt.Sprintf("Download of %s was cancelled: %s", filename, reason),
	}
}

// NewFileNotFoundError creates a new error for file not found situations
func NewFileNotFoundError(fileID int64, path string) error {
	return &DownloadError{
		Type:    "FileNotFound",
		Message: fmt.Sprintf("File ID %d not found at path: %s", fileID, path),
	}
}

// NewDownloadStalledError creates a new error for stalled downloads
func NewDownloadStalledError(filename string, duration time.Duration) error {
	return &DownloadError{
		Type:    "DownloadStalled",
		Message: fmt.Sprintf("Download of %s stalled for %v", filename, duration),
	}
}

// NewTransferNotFoundError creates a new error for transfer not found situations
func NewTransferNotFoundError(transferID int64) error {
	return &DownloadError{
		Type:    "TransferNotFound",
		Message: fmt.Sprintf("Transfer ID %d not found", transferID),
	}
}

// NewInvalidContentLengthError creates a new error for invalid content length responses
func NewInvalidContentLengthError(filename string, length int64) error {
	return &DownloadError{
		Type:    "InvalidContentLength",
		Message: fmt.Sprintf("Invalid content length %d for file: %s", length, filename),
	}
}

// NewNoFilesFoundError creates a new error for transfers with no files
func NewNoFilesFoundError(transferID int64) error {
	return &DownloadError{
		Type:    "NoFilesFound",
		Message: fmt.Sprintf("No files found for transfer %d", transferID),
	}
}
