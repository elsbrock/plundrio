package download

import (
	"io"
	"sync"
	"time"
)

// downloadJob represents a single download task
type downloadJob struct {
	FileID     int64
	Name       string
	IsFolder   bool
	TransferID int64 // Parent transfer ID for group tracking
}

// DownloadState tracks the progress of a file download
type DownloadState struct {
	TransferID   int64
	FileID       int64
	Name         string
	Progress     float64
	ETA          time.Time
	LastProgress time.Time
	StartTime    time.Time

	// Mutex to protect access to downloaded bytes counter
	mu         sync.Mutex
	downloaded int64
}

// progressReader wraps an io.Reader to track download progress
type progressReader struct {
	reader       io.Reader
	onProgress   func(n int64)
	startTime    time.Time
	initialBytes int64
}

// Read implements io.Reader for progressReader
func (r *progressReader) Read(p []byte) (n int, err error) {
	n, err = r.reader.Read(p)
	if n > 0 && r.onProgress != nil {
		r.onProgress(int64(n))
	}
	return
}

// TransferLifecycleState represents the possible states of a transfer
type TransferLifecycleState int32

const (
	TransferLifecycleInitial TransferLifecycleState = iota
	TransferLifecycleDownloading
	TransferLifecycleCompleted
	TransferLifecycleFailed
	TransferLifecycleCancelled
)

// String returns a string representation of the transfer state
func (s TransferLifecycleState) String() string {
	switch s {
	case TransferLifecycleInitial:
		return "Initial"
	case TransferLifecycleDownloading:
		return "Downloading"
	case TransferLifecycleCompleted:
		return "Completed"
	case TransferLifecycleFailed:
		return "Failed"
	case TransferLifecycleCancelled:
		return "Cancelled"
	default:
		return "Unknown"
	}
}

// TransferContext tracks the complete state of a transfer
type TransferContext struct {
	ID             int64
	Name           string
	FileID         int64
	TotalFiles     int32
	CompletedFiles int32
	FailedFiles    int32 // Track number of failed files
	State          TransferLifecycleState
	Error          error
	mu             sync.RWMutex
}
