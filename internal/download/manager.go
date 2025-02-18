// Package download provides functionality for managing downloads from Put.io.
// It handles concurrent downloads, progress tracking, and cleanup of completed transfers.
package download

import (
	"sync"
	"time"

	"github.com/elsbrock/plundrio/internal/api"
	"github.com/elsbrock/plundrio/internal/config"
)

const (
	defaultWorkerCount       = 4
	progressUpdateInterval   = 5 * time.Second
	downloadStallTimeout    = 1 * time.Minute
	downloadHeaderTimeout   = 30 * time.Second
	idleConnectionTimeout   = 60 * time.Second
	downloadBufferMultiple  = 2 // Buffer size multiplier for download jobs channel
)

// Manager handles downloading completed transfers from Put.io.
// It supports concurrent downloads, progress tracking, and automatic cleanup
// of completed transfers. The manager uses a worker pool pattern to process
// downloads efficiently while maintaining control over system resources.
type Manager struct {
	cfg         *config.Config
	client      *api.Client
	active      sync.Map // map[int64]*DownloadState
	activeFiles sync.Map // map[int64]bool - tracks files being downloaded
	stopChan    chan struct{}
	wg          sync.WaitGroup
	jobs        chan downloadJob
	mu          sync.Mutex // protects job queueing

	// Transfer tracking
	transferFiles       map[int64]int  // Track total files per transfer
	completedTransfers map[int64]bool // Track completed transfers
	transferMutex      sync.Mutex
}

// New creates a new download manager
func New(cfg *config.Config, client *api.Client) *Manager {
	workerCount := cfg.WorkerCount
	if workerCount <= 0 {
		workerCount = defaultWorkerCount
	}

	m := &Manager{
		cfg:                cfg,
		client:            client,
		stopChan:          make(chan struct{}),
		jobs:              make(chan downloadJob, workerCount*downloadBufferMultiple),
		activeFiles:       sync.Map{},
		transferFiles:     make(map[int64]int),
		completedTransfers: make(map[int64]bool),
	}

	return m
}

// Start begins monitoring transfers and downloading completed ones
func (m *Manager) Start() {
	workerCount := m.cfg.WorkerCount
	if workerCount <= 0 {
		workerCount = 4
	}

	// Start download workers
	for i := 0; i < workerCount; i++ {
		m.wg.Add(1)
		go m.downloadWorker()
	}

	// Start transfer monitor
	m.wg.Add(1)
	go m.monitorTransfers()
}

// Stop gracefully shuts down the manager
func (m *Manager) Stop() {
	close(m.stopChan)
	m.wg.Wait()
}

// QueueDownload adds a download job to the queue if not already downloading
func (m *Manager) QueueDownload(job downloadJob) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if file is already being downloaded
	if _, exists := m.activeFiles.Load(job.FileID); exists {
		return
	}

	// Mark file as being downloaded before queueing
	m.activeFiles.Store(job.FileID, true)
	select {
	case m.jobs <- job:
		// Successfully queued
	case <-m.stopChan:
		// Manager is shutting down, remove from active files
		m.activeFiles.Delete(job.FileID)
	}
}
