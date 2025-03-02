package download

import (
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

// ProcessedStatus represents whether a transfer has been processed locally
type ProcessedStatus bool

const (
	// NotProcessed indicates the transfer has not been processed locally
	NotProcessed ProcessedStatus = false
	// Processed indicates the transfer has been processed locally
	Processed ProcessedStatus = true
)

// TransferContext tracks the complete state of a transfer
type TransferContext struct {
	ID             int64
	Name           string
	FileID         int64
	TotalFiles     int32
	CompletedFiles int32
	FailedFiles    int32           // Track number of failed files
	TotalSize      int64           // Total size of all files in bytes
	DownloadedSize int64           // Total downloaded bytes
	Processed      ProcessedStatus // Whether this transfer has been processed locally
	mu             sync.RWMutex
}

// TransferCache caches put.io transfer information for RPC
type TransferCache struct {
	transfers   map[string]*PutioTransfer // Map of hash -> transfer
	hashToID    map[string]int64          // Map of hash -> transfer ID
	idToHash    map[int64]string          // Map of transfer ID -> hash
	lastUpdated time.Time                 // When the cache was last updated
	updateLock  sync.RWMutex              // Lock for cache updates
}

// PutioTransfer represents a cached transfer from put.io
type PutioTransfer struct {
	ID             int64     // Transfer ID
	Hash           string    // Transfer hash
	Name           string    // Transfer name
	Status         string    // Put.io status (IN_QUEUE, WAITING, etc.)
	Size           int64     // Total size in bytes
	Downloaded     int64     // Bytes downloaded on put.io
	DownloadedSize int64     // Bytes downloaded locally
	FileID         int64     // File ID on put.io
	CreatedAt      time.Time // When the transfer was created
	FinishedAt     time.Time // When the transfer finished on put.io
	SecondsSeeding int       // How long the transfer has been seeding
	ErrorMessage   string    // Error message if any
	Availability   int       // Peer availability
	PercentDone    int       // Percent done on put.io (0-100)
}

// ProgressTracker handles two-phase progress calculation
type ProgressTracker struct {
	cache       map[int64]float64    // Cache of transfer ID -> progress
	cacheLock   sync.RWMutex         // Lock for cache access
	processor   *TransferProcessor   // Reference to transfer processor
	coordinator *TransferCoordinator // Reference to transfer coordinator
}
