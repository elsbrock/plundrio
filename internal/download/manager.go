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
	defaultWorkerCount     = 4
	progressUpdateInterval = 5 * time.Second
	downloadStallTimeout   = 1 * time.Minute
	downloadHeaderTimeout  = 30 * time.Second
	idleConnectionTimeout  = 60 * time.Second
	downloadBufferMultiple = 2 // Buffer size multiplier for download jobs channel
)

// Manager handles downloading completed transfers from Put.io.
// It supports concurrent downloads, progress tracking, and automatic cleanup
// of completed transfers. The manager uses a worker pool pattern to process
// downloads efficiently while maintaining control over system resources.
type Manager struct {
	cfg         *config.Config
	client      *api.Client
	active      sync.Map // map[int64]*DownloadState
	activeFiles sync.Map // map[int64]int64 - tracks files being downloaded, FileID -> TransferID
	stopChan    chan struct{}
	stopOnce    sync.Once
	workerWg    sync.WaitGroup // tracks worker goroutines
	monitorWg   sync.WaitGroup // tracks monitor goroutine
	jobs        chan downloadJob
	mu          sync.Mutex // protects job queueing
	running     bool       // tracks if manager is running

	// Transfer tracking
	transferFiles      map[int64]int    // Track total files per transfer
	transferNames      map[int64]string // Track transfer names
	completedTransfers map[int64]bool   // Track completed transfers
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
		client:             client,
		stopChan:           make(chan struct{}),
		jobs:               make(chan downloadJob, workerCount*downloadBufferMultiple),
		activeFiles:        sync.Map{},
		transferFiles:      make(map[int64]int),
		transferNames:      make(map[int64]string),
		completedTransfers: make(map[int64]bool),
	}

	return m
}

// Start begins monitoring transfers and downloading completed ones
func (m *Manager) Start() {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return
	}
	m.running = true
	m.mu.Unlock()

	workerCount := m.cfg.WorkerCount
	if workerCount <= 0 {
		workerCount = 4
	}

	// Add to WaitGroup before starting goroutines
	m.workerWg.Add(workerCount)
	m.monitorWg.Add(1)

	// Start download workers with proper synchronization
	for i := 0; i < workerCount; i++ {
		go m.downloadWorker()
	}

	// Repopulate transferFiles map from activeFiles with proper locking
	m.transferMutex.Lock()
	m.activeFiles.Range(func(key, value interface{}) bool {
		transferID := value.(int64)
		m.transferFiles[transferID]++
		return true
	})
	m.transferMutex.Unlock()

	// Start transfer monitor
	go func() {
		defer m.monitorWg.Done()
		m.monitorTransfers()
	}()
}

// Stop gracefully shuts down the manager
func (m *Manager) Stop() {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return
	}
	m.running = false
	m.mu.Unlock()

	m.stopOnce.Do(func() {
		close(m.stopChan)
		// Drain any remaining jobs to prevent deadlock
		go func() {
			for range m.jobs {
				// Drain jobs channel
			}
		}()
	})

	// Wait for all workers to finish
	m.workerWg.Wait()
	// Close jobs channel after workers are done
	close(m.jobs)
	// Wait for monitor to finish
	m.monitorWg.Wait()
}

// markTransferCompleted safely marks a transfer as completed
func (m *Manager) markTransferCompleted(transferID int64) {
	m.transferMutex.Lock()
	defer m.transferMutex.Unlock()
	m.completedTransfers[transferID] = true
}

// handleFileCompletion updates transfer state when a file finishes
func (m *Manager) handleFileCompletion(transferID int64) {
	m.transferMutex.Lock()
	defer m.transferMutex.Unlock()

	if count, exists := m.transferFiles[transferID]; exists && count > 0 {
		m.transferFiles[transferID]--
	}
}

// QueueDownload adds a download job to the queue if not already downloading
func (m *Manager) QueueDownload(job downloadJob) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if file is already being downloaded
	if _, exists := m.activeFiles.Load(job.FileID); exists {
		return
	}

	// Mark file as being downloaded before queueing, storing TransferID
	m.activeFiles.Store(job.FileID, job.TransferID)
	select {
	case m.jobs <- job:
		// Successfully queued
	case <-m.stopChan:
		// Manager is shutting down, remove from active files
		m.activeFiles.Delete(job.FileID)
	}
}
