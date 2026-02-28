package download

import (
	"sync"
	"time"
)

// downloadJob represents a single download task
type downloadJob struct {
	FileID     int64
	Name       string
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

// TransferLifecycleState represents the possible states of a transfer
type TransferLifecycleState int32

const (
	TransferLifecycleInitial TransferLifecycleState = iota
	TransferLifecycleDownloading
	TransferLifecycleCompleted
	TransferLifecycleFailed
	TransferLifecycleCancelled
	TransferLifecycleProcessed // Transfer has been processed locally and can be shown as 100% complete
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
	case TransferLifecycleProcessed:
		return "Processed"
	default:
		return "Unknown"
	}
}

// TransferContext tracks the complete state of a transfer.
// Write-once fields (ID, Name, FileID, TotalFiles) are safe to read without locking.
// All mutable fields are unexported and accessed through thread-safe methods.
type TransferContext struct {
	ID         int64
	Name       string
	FileID     int64
	TotalFiles int32

	// Mutable fields — access only via methods or under mu from same package.
	completedFiles int32
	failedFiles    int32
	totalSize      int64   // Total size of all files in bytes
	downloadedSize int64   // Total downloaded bytes
	localSpeed     float64 // Current local download speed in bytes/sec
	localETA       time.Time
	state          TransferLifecycleState
	err            error
	mu             sync.RWMutex
}

// NewTransferContext creates a TransferContext for use in tests or cross-package setup.
func NewTransferContext(id int64, totalFiles int32, state TransferLifecycleState) *TransferContext {
	return &TransferContext{
		ID:         id,
		TotalFiles: totalFiles,
		state:      state,
	}
}

// AddDownloadedBytes atomically adds delta to the downloaded byte count.
func (tc *TransferContext) AddDownloadedBytes(delta int64) {
	tc.mu.Lock()
	tc.downloadedSize += delta
	tc.mu.Unlock()
}

// SetLocalProgress sets the local download speed and ETA under one lock.
func (tc *TransferContext) SetLocalProgress(speed float64, eta time.Time) {
	tc.mu.Lock()
	tc.localSpeed = speed
	tc.localETA = eta
	tc.mu.Unlock()
}

// SetTotalSize sets the total transfer size in bytes.
func (tc *TransferContext) SetTotalSize(size int64) {
	tc.mu.Lock()
	tc.totalSize = size
	tc.mu.Unlock()
}

// GetProgress returns a snapshot of download progress counters.
func (tc *TransferContext) GetProgress() (downloadedSize, totalSize int64, completedFiles, failedFiles int32) {
	tc.mu.RLock()
	downloadedSize = tc.downloadedSize
	totalSize = tc.totalSize
	completedFiles = tc.completedFiles
	failedFiles = tc.failedFiles
	tc.mu.RUnlock()
	return
}

// GetLocalProgress returns the current local download speed and ETA.
func (tc *TransferContext) GetLocalProgress() (speed float64, eta time.Time) {
	tc.mu.RLock()
	speed = tc.localSpeed
	eta = tc.localETA
	tc.mu.RUnlock()
	return
}

// GetState returns the current lifecycle state.
func (tc *TransferContext) GetState() TransferLifecycleState {
	tc.mu.RLock()
	s := tc.state
	tc.mu.RUnlock()
	return s
}

// GetError returns the current error, if any.
func (tc *TransferContext) GetError() error {
	tc.mu.RLock()
	e := tc.err
	tc.mu.RUnlock()
	return e
}
