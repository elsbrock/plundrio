package download

import "time"

// DownloadConfig contains configuration options for the download manager
type DownloadConfig struct {
	// DefaultWorkerCount is the default number of concurrent download workers
	DefaultWorkerCount int

	// BufferMultiple is used to calculate the job channel buffer size (workerCount * BufferMultiple)
	BufferMultiple int

	// ProgressUpdateInterval is how often download progress is logged
	ProgressUpdateInterval time.Duration

	// TransferCheckInterval is how often to check for new transfers
	TransferCheckInterval time.Duration

	// IdleConnectionTimeout is the maximum amount of time an idle connection is kept open
	IdleConnectionTimeout time.Duration

	// DownloadHeaderTimeout is the timeout for receiving the response headers
	DownloadHeaderTimeout time.Duration

	// DownloadStallTimeout is how long a download can stall before being cancelled
	DownloadStallTimeout time.Duration

	// CopyTimeout is the timeout for waiting for the copy operation to complete after cancellation
	CopyTimeout time.Duration
}

// GetDefaultConfig returns a DownloadConfig with reasonable default values
func GetDefaultConfig() *DownloadConfig {
	return &DownloadConfig{
		DefaultWorkerCount:     3,                // 3 concurrent downloads by default
		BufferMultiple:         5,                // Buffer size = 5 * worker count
		ProgressUpdateInterval: 5 * time.Second,  // Log progress every 5 seconds
		TransferCheckInterval:  30 * time.Second, // Check for new transfers every 30 seconds
		IdleConnectionTimeout:  90 * time.Second, // Keep idle connections for 90 seconds
		DownloadHeaderTimeout:  30 * time.Second, // 30 second timeout for response headers
		DownloadStallTimeout:   2 * time.Minute,  // Cancel download if stalled for 2 minutes
		CopyTimeout:            10 * time.Second, // Wait 10 seconds for copy to complete after cancellation
	}
}
