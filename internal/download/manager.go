package download

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/config"
	"github.com/elsbrock/plundrio/internal/log"
)

// PutioClient abstracts the put.io API methods used by the download manager.
type PutioClient interface {
	GetTransfers(ctx context.Context) ([]*putio.Transfer, error)
	GetAllTransferFiles(ctx context.Context, fileID int64) ([]*putio.File, error)
	GetFiles(ctx context.Context, folderID int64) ([]*putio.File, error)
	RetryTransfer(ctx context.Context, transferID int64) (*putio.Transfer, error)
	DeleteTransfer(ctx context.Context, transferID int64) error
	DeleteFile(ctx context.Context, fileID int64) error
	GetDownloadURL(ctx context.Context, fileID int64) (string, error)
}

// Manager handles downloading completed transfers from Put.io.
// It supports concurrent downloads, progress tracking, and automatic cleanup
// of completed transfers. The manager uses a worker pool pattern to process
// downloads efficiently while maintaining control over system resources.
type Manager struct {
	cfg      *config.Config
	client   PutioClient
	dlConfig *DownloadConfig // Download-specific configuration

	coordinator           *TransferCoordinator // Coordinates transfer lifecycle
	categories            *CategoryStore       // Maps transfer hash → category subfolder
	transferFiles         *TransferFileStore   // Persists exact local files by transfer ID
	removalMu             sync.RWMutex         // Serializes removal with state publication and queue admission
	activeFiles           sync.Map             // map[int64]int64 - tracks files being downloaded, FileID -> TransferID
	downloadRetryAttempts sync.Map             // map[int64]int - bounded local retry rounds by FileID
	downloadRetryDelay    func(int) (time.Duration, bool)

	ctx    context.Context
	cancel context.CancelFunc

	stopChan chan struct{}
	stopOnce sync.Once

	// Shutdown happens in dependency order: the monitor spawns transfer
	// processors, which queue jobs for the workers.
	monitorWg   sync.WaitGroup // tracks the transfer monitor goroutine
	processorWg sync.WaitGroup // tracks per-transfer processing goroutines
	workerWg    sync.WaitGroup // tracks download worker goroutines

	// jobs is never closed; workers wind down via stopChan. Closing it would
	// panic any in-flight QueueDownload racing the shutdown.
	jobs chan downloadJob

	mu      sync.Mutex // guards running
	running bool       // tracks if manager is running

	processor *TransferProcessor // Handles transfer processing
}

// Context returns the manager's lifecycle context.
// Safe to call before Start (returns context.Background as fallback).
func (m *Manager) Context() context.Context {
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}

// GetTransfers returns all tracked transfers for the configured folder.
func (m *Manager) GetTransfers() []*putio.Transfer {
	if m.processor == nil {
		return nil
	}
	return m.processor.GetTransfers()
}

// GetTransferContext returns the lifecycle context for a transfer, if tracked.
func (m *Manager) GetTransferContext(transferID int64) (*TransferContext, bool) {
	return m.coordinator.GetTransferContext(transferID)
}

// GetTransferFiles returns the exact persisted file list for a transfer.
func (m *Manager) GetTransferFiles(transferID int64) ([]TransferFile, bool) {
	return m.transferFiles.Get(transferID)
}

// SetCategory stores a category for a put.io transfer ID.
func (m *Manager) SetCategory(transferID int64, category string) {
	m.categories.Set(transferID, category)
}

// GetCategory returns the category for a put.io transfer ID, or "" if none.
func (m *Manager) GetCategory(transferID int64) string {
	if m.RemovalPending(transferID) {
		category, _ := m.removalCategory(transferID)
		return category
	}
	return m.categories.Get(transferID)
}

// RemoveCategory deletes the stored category for a put.io transfer ID.
func (m *Manager) RemoveCategory(transferID int64) {
	m.categories.Remove(transferID)
}

// localCategory returns the category subfolder to use for a transfer's local
// path, or "" when local categorization is disabled.
func (m *Manager) localCategory(transferID int64) string {
	if !m.cfg.UseCategoriesTarget {
		return ""
	}
	return m.GetCategory(transferID)
}

// RemoveTransfer stops tracking a transfer and drops its local bookkeeping.
// Called once *arr removes the torrent; without it, tracking state would grow
// for the lifetime of the process.
func (m *Manager) RemoveTransfer(transferID int64) {
	m.removalMu.Lock()
	defer m.removalMu.Unlock()
	m.processor.forget(transferID)
	// In-flight workers must finish before removing their suppression marker.
	if m.activeFileCount(transferID) > 0 && m.RemovalPending(transferID) {
		return
	}
	if err := m.transferFiles.Remove(transferID); err != nil {
		log.Error("files").
			Int64("transfer_id", transferID).
			Err(err).
			Msg("Failed to remove transfer file state")
		return
	}
	if err := m.removeReview(transferID); err != nil {
		log.Error("files").Int64("transfer_id", transferID).Err(err).Msg("Failed to remove review hold")
		return
	}
	if err := os.Remove(m.removalPath(transferID)); err != nil && !os.IsNotExist(err) {
		log.Error("files").Err(err).Msg("Failed to remove transfer removal marker")
	}
}

// activeFileCount returns how many of a transfer's files are still downloading.
func (m *Manager) activeFileCount(transferID int64) int {
	count := 0
	m.activeFiles.Range(func(_, value interface{}) bool {
		if value.(int64) == transferID {
			count++
		}
		return true
	})
	return count
}

// New creates a new download manager
func New(cfg *config.Config, client PutioClient) *Manager {
	// Get default download configuration
	dlConfig := GetDefaultConfig()

	// Override with user config if provided
	workerCount := cfg.WorkerCount
	if workerCount <= 0 {
		workerCount = dlConfig.DefaultWorkerCount
	}

	m := &Manager{
		cfg:                cfg,
		client:             client,
		dlConfig:           dlConfig,
		categories:         newCategoryStore(cfg.TargetDir),
		transferFiles:      newTransferFileStore(cfg.TargetDir),
		stopChan:           make(chan struct{}),
		jobs:               make(chan downloadJob, workerCount*dlConfig.BufferMultiple),
		activeFiles:        sync.Map{},
		downloadRetryDelay: downloadRetryDelay,
	}

	// Initialize coordinator and processor
	m.processor = newTransferProcessor(m)
	m.coordinator = NewTransferCoordinator()

	// Register cleanup hooks
	m.coordinator.RegisterCleanupHook(func(transferID int64) error {
		state, ok := m.coordinator.GetTransferContext(transferID)
		if !ok {
			return NewTransferNotFoundError(transferID)
		}
		// Put.io uses zero for the root folder. Completed transfer records can
		// remain after their source file has been deleted, so never pass that
		// sentinel to DeleteFile after a restart.
		if state.FileID == 0 {
			log.Debug("cleanup").
				Int64("transfer_id", transferID).
				Msg("Skipping source deletion: transfer has no associated file")
			return nil
		}

		// Delete only the source file from Put.io, but keep the transfer
		if err := m.client.DeleteFile(m.Context(), state.FileID); err != nil {
			log.Error("cleanup").
				Int64("transfer_id", transferID).
				Int64("file_id", state.FileID).
				Err(err).
				Msg("Failed to delete source file")
			return err
		}

		log.Info("cleanup").
			Int64("transfer_id", transferID).
			Msg("Deleted source file")

		return nil
	})

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

	m.ctx, m.cancel = context.WithCancel(context.Background())

	m.categories.Load()

	workerCount := m.cfg.WorkerCount
	if workerCount <= 0 {
		workerCount = m.dlConfig.DefaultWorkerCount
	}

	// Start download workers with proper synchronization
	for i := 0; i < workerCount; i++ {
		m.workerWg.Add(1)
		go func() {
			defer m.workerWg.Done()
			m.downloadWorker()
		}()
	}

	// Start transfer monitor
	m.monitorWg.Add(1)
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
		// Cancel the context first so in-flight API calls and downloads abort,
		// then signal the monitor and workers to wind down.
		m.cancel()
		close(m.stopChan)
	})

	// Wait in dependency order. The monitor adds to processorWg and the
	// processors add to workerWg, so draining in any other order would let an
	// Add race a Wait.
	m.monitorWg.Wait()
	m.processorWg.Wait()
	m.workerWg.Wait()
}

// QueueDownload adds a download job to the queue if not already downloading.
//
// This deliberately holds no lock across the channel send: the send blocks
// once the queue is full, and blocking there while holding m.mu would
// deadlock against Stop.
func (m *Manager) QueueDownload(job downloadJob) {
	ctx, _ := m.coordinator.GetTransferContext(job.TransferID)
	m.queueDownload(job, ctx)
}

func (m *Manager) queueDownload(job downloadJob, expected *TransferContext) {
	if !m.claimDownload(job, expected) {
		return
	}
	// The active claim survives a blocked send, retaining suppression until
	// a worker sees and discards the job if removal happened in the meantime.
	select {
	case m.jobs <- job:
	case <-m.stopChan:
		m.activeFiles.Delete(job.FileID)
	}
}

func (m *Manager) claimDownload(job downloadJob, expected *TransferContext) bool {
	m.removalMu.RLock()
	defer m.removalMu.RUnlock()
	current, exists := m.coordinator.GetTransferContext(job.TransferID)
	if !exists || expected == nil || current != expected {
		m.downloadRetryAttempts.Delete(job.FileID)
		return false
	}
	if m.RemovalPending(job.TransferID) {
		m.downloadRetryAttempts.Delete(job.FileID)
		return false
	}
	// Refuse new work once shutdown has begun. This is checked before claiming
	// the file because the queue is buffered: after the workers exit, a send
	// still succeeds, which would otherwise leave the file marked active with
	// nothing left to run it.
	select {
	case <-m.stopChan:
		return false
	default:
	}

	// LoadOrStore makes "not already downloading" and claiming the file a
	// single atomic step.
	if _, alreadyActive := m.activeFiles.LoadOrStore(job.FileID, job.TransferID); alreadyActive {
		return false
	}
	return true
}

// cleanupTransfer handles the deletion of a completed transfer and its source files
func (m *Manager) cleanupTransfer(transferID int64) {
	// Get transfer state before cleanup
	ctx, ok := m.coordinator.GetTransferContext(transferID)
	if !ok {
		log.Debug("transfers").
			Int64("id", transferID).
			Msg("Transfer not found during cleanup")
		return
	}

	log.Debug("transfers").
		Str("name", ctx.Name).
		Int64("id", transferID).
		Int64("file_id", ctx.FileID).
		Msg("Cleaning up transfer")

	// Complete the transfer in the coordinator, which will run cleanup hooks
	if err := m.coordinator.CompleteTransfer(transferID); err != nil {
		log.Error("cleanup").
			Int64("transfer_id", transferID).
			Err(err).
			Msg("Failed to complete transfer")
	}

	log.Info("transfers").
		Str("name", ctx.Name).
		Int64("id", transferID).
		Msg("Cleaned up transfer")
}

// handleFileCompletion updates transfer state when a file completes downloading
func (m *Manager) handleFileCompletion(transferID int64, fileID int64) {
	// First increment the completion counter in the transfer coordinator
	if err := m.coordinator.FileCompleted(transferID); err != nil {
		log.Error("transfers").
			Int64("transfer_id", transferID).
			Int64("file_id", fileID).
			Err(err).
			Msg("Failed to handle file completion")
		return
	}

	log.Debug("transfers").
		Int64("transfer_id", transferID).
		Int64("file_id", fileID).
		Msg("File marked as completed")

	// Now that the counter has been incremented, remove the file from active tracking
	m.activeFiles.Delete(fileID)

	// Check if the transfer is marked as completed
	ctx, ok := m.coordinator.GetTransferContext(transferID)
	if !ok {
		log.Debug("transfers").
			Int64("transfer_id", transferID).
			Msg("Transfer context not found after completion")
		return
	}

	state := ctx.GetState()
	_, _, completedFiles, _ := ctx.GetProgress()

	log.Debug("transfers").
		Int64("id", transferID).
		Int32("completed_files", completedFiles).
		Int32("total_files", ctx.TotalFiles).
		Bool("is_completed_state", state == TransferLifecycleCompleted).
		Msg("Transfer completion status")

	// If the transfer is in completed state, check if all downloads are done
	if state == TransferLifecycleCompleted {
		activeCount := m.activeFileCount(transferID)

		log.Debug("transfers").
			Int64("id", transferID).
			Int("active_files", activeCount).
			Msg("Active files for completed transfer")

		// Only if no active files remain for this transfer, finalize it
		if activeCount == 0 {
			log.Info("transfers").
				Int64("id", transferID).
				Msg("All downloads complete, finalizing transfer")

			if err := m.coordinator.CompleteTransfer(transferID); err != nil {
				log.Error("transfers").
					Int64("id", transferID).
					Err(err).
					Msg("Failed to finalize completed transfer")
			}
		}
	}
}

// handleFileFailure marks a file as failed in the transfer context
func (m *Manager) handleFileFailure(transferID int64) {
	if err := m.coordinator.FileFailure(transferID); err != nil {
		log.Error("transfers").
			Int64("transfer_id", transferID).
			Err(err).
			Msg("Failed to handle file failure")
	}
}
