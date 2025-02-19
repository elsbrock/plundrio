package download

import (
	"io"
	"time"
)

// ErrDownloadStalled is returned when a download makes no progress for too long
type ErrDownloadStalled struct {
	Filename string
	Duration time.Duration
}

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
	Status       string
	Progress     float64
	ETA          time.Time
	LastProgress time.Time
	StartTime    time.Time
}

// progressReader wraps an io.Reader to track download progress
type progressReader struct {
	reader       io.Reader
	onProgress   func(n int64)
	startTime    time.Time
	initialBytes int64
}

func (r *progressReader) Read(p []byte) (n int, err error) {
	n, err = r.reader.Read(p)
	if n > 0 && r.onProgress != nil {
		r.onProgress(int64(n))
	}
	return
}
