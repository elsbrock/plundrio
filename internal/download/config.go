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

	// SeedingTimeThreshold is how long a transfer should seed before being cancelled
	SeedingTimeThreshold time.Duration

	// AvailabilityThreshold is the minimum availability required for a transfer
	AvailabilityThreshold int

	// CacheUpdateInterval is how often the transfer cache is updated
	CacheUpdateInterval time.Duration

	// MaxRetryAttempts is the maximum number of times to retry a failed transfer
	MaxRetryAttempts int
}

// GetDefaultConfig returns a DownloadConfig with reasonable default values
func GetDefaultConfig() *DownloadConfig {
	return &DownloadConfig{
		DefaultWorkerCount:     3,               // 3 concurrent downloads by default
		BufferMultiple:         5,               // Buffer size = 5 * worker count
		ProgressUpdateInterval: 5 * time.Second, // Log progress every 5 seconds
		SeedingTimeThreshold:   24 * time.Hour,  // Seed for 24 hours by default
		AvailabilityThreshold:  5,               // Minimum availability of 5 peers
		CacheUpdateInterval:    5 * time.Minute, // Update transfer cache every 5 minutes
		MaxRetryAttempts:       3,               // Retry failed transfers up to 3 times
	}
}
