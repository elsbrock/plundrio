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

	// DeleteBeforeCompleted controls whether to delete files and transfers before they reach completed state
	// If true, files will be deleted as soon as possible (even during seeding)
	// If false, files will only be deleted once they reach completed state
	// Default is true to maintain backward compatibility
	DeleteBeforeCompleted bool
}
