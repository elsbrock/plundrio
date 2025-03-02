package config

// Config holds the runtime configuration
type Config struct {
	// TargetDir is where completed downloads will be stored
	TargetDir string

	// PutioFolder is the name of the folder in Put.io
	PutioFolder string

	// FolderID is the Put.io folder ID (set after creation/lookup)
	FolderID int64

	// OAuthToken is the Put.io OAuth token
	OAuthToken string

	// ListenAddr is the address to listen for transmission-rpc requests
	ListenAddr string

	// WorkerCount is the number of concurrent download workers (default: 4)
	WorkerCount int

	// LogLevel is the logging level (trace,debug,info,warn,error,fatal,none,pretty)
	LogLevel string

	// TransferCheckInterval is how often to check for new transfers (default: 30s)
	TransferCheckInterval string

	// DownloadStallTimeout is how long a download can stall before being cancelled (default: 2m)
	DownloadStallTimeout string

	// SeedingTimeThreshold is how long a transfer should seed before being cancelled (default: 24h)
	SeedingTimeThreshold string

	// MaxRetryAttempts is the maximum number of times to retry a failed transfer (default: 3)
	MaxRetryAttempts int
}
