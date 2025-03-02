package download

import (
	"sync"
	"time"

	"github.com/elsbrock/plundrio/internal/api"
	"github.com/elsbrock/plundrio/internal/config"
	"github.com/elsbrock/plundrio/internal/log"
)

// Manager handles downloading completed transfers from Put.io.
// It supports concurrent downloads, progress tracking, and automatic cleanup
// of completed transfers. The manager uses a worker pool pattern to process
// downloads efficiently while maintaining control over system resources.
type Manager struct {
	cfg      *config.Config
	client   *api.Client
	dlConfig *DownloadConfig // Download-specific configuration

	coordinator *TransferCoordinator // Coordinates transfer lifecycle
	activeFiles sync.Map             // map[int64]int64 - tracks files being downloaded, FileID -> TransferID

	stopChan chan struct{}
	stopOnce sync.Once

	workerWg  sync.WaitGroup // tracks worker goroutines
	monitorWg sync.WaitGroup // tracks monitor goroutine

	jobs    chan downloadJob
	mu      sync.Mutex // protects job queueing
	running bool       // tracks if manager is running

	processor  *TransferProcessor // Handles transfer processing
	rpcHandler *RPCHandler        // Handles RPC requests
}

// New creates a new download manager
func New(cfg *config.Config, client *api.Client) *Manager {
	// Get default download configuration
	dlConfig := GetDefaultConfig()

	// Override with user config if provided
	workerCount := cfg.WorkerCount
	if workerCount <= 0 {
		workerCount = dlConfig.DefaultWorkerCount
	}

	// Parse and apply other configuration parameters if provided
	if cfg.TransferCheckInterval != "" {
		if duration, err := time.ParseDuration(cfg.TransferCheckInterval); err == nil {
			dlConfig.TransferCheckInterval = duration
			log.Debug("config").
				Str("transfer_check_interval", cfg.TransferCheckInterval).
				Msg("Using custom transfer check interval")
		} else {
			log.Warn("config").
				Str("transfer_check_interval", cfg.TransferCheckInterval).
				Err(err).
				Msg("Invalid transfer check interval, using default")
		}
	}

	if cfg.SeedingTimeThreshold != "" {
		if duration, err := time.ParseDuration(cfg.SeedingTimeThreshold); err == nil {
			dlConfig.SeedingTimeThreshold = duration
			log.Debug("config").
				Str("seeding_time_threshold", cfg.SeedingTimeThreshold).
				Msg("Using custom seeding time threshold")
		} else {
			log.Warn("config").
				Str("seeding_time_threshold", cfg.SeedingTimeThreshold).
				Err(err).
				Msg("Invalid seeding time threshold, using default")
		}
	}

	if cfg.MaxRetryAttempts > 0 {
		dlConfig.MaxRetryAttempts = cfg.MaxRetryAttempts
		log.Debug("config").
			Int("max_retry_attempts", cfg.MaxRetryAttempts).
			Msg("Using custom max retry attempts")
	}

	m := &Manager{
		cfg:         cfg,
		client:      client,
		dlConfig:    dlConfig,
		stopChan:    make(chan struct{}),
		jobs:        make(chan downloadJob, workerCount*dlConfig.BufferMultiple),
		activeFiles: sync.Map{},
	}

	// Initialize coordinator and processor
	m.coordinator = NewTransferCoordinator(m)
	m.processor = newTransferProcessor(m)

	// Initialize RPC handler after processor is created
	m.rpcHandler = NewRPCHandler(m.processor)

	// Register cleanup hooks
	m.coordinator.RegisterCleanupHook(func(transferID int64) error {
		state, ok := m.coordinator.GetTransferContext(transferID)
		if !ok {
			return NewTransferNotFoundError(transferID)
		}

		// Delete only the source file from Put.io, but keep the transfer
		// This allows *arr applications to see completed transfers
		if err := m.client.DeleteFile(state.FileID); err != nil {
			log.Error("cleanup").
				Int64("transfer_id", transferID).
				Int64("file_id", state.FileID).
				Err(err).
				Msg("Failed to delete source file")
			return err
		}

		// No longer delete the transfer - it will only be deleted when torrent-remove is called
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
		// Signal workers to stop via stopChan
		close(m.stopChan)
		// Close jobs channel to prevent new submissions
		close(m.jobs)
		// Drain any remaining jobs to prevent deadlock
		go func() {
			for range m.jobs {
				// Drain jobs channel
			}
		}()
	})

	// Wait for all workers to finish
	m.workerWg.Wait()
	// Wait for monitor to finish
	m.monitorWg.Wait()
}

// GetTransferProcessor returns the manager's transfer processor
func (m *Manager) GetTransferProcessor() *TransferProcessor {
	return m.processor
}

// GetRPCHandler returns the manager's RPC handler
func (m *Manager) GetRPCHandler() *RPCHandler {
	return m.rpcHandler
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
		// Manager is shutting down, just remove from active files
		m.activeFiles.Delete(job.FileID)
	}
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
// This is called for successful downloads only with the specific fileID that completed
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

	// Log detailed completion info
	log.Debug("transfers").
		Int64("transfer_id", transferID).
		Int64("file_id", fileID).
		Msg("File marked as completed")

	// Now that the counter has been incremented, remove the file from active tracking
	m.activeFiles.Delete(fileID)

	// Check if the transfer is ready for completion
	ctx, ok := m.coordinator.GetTransferContext(transferID)
	if !ok {
		log.Debug("transfers").
			Int64("transfer_id", transferID).
			Msg("Transfer context not found after completion")
		return // Transfer context already gone
	}

	// Get transfer info under lock
	ctx.mu.RLock()
	totalFiles := ctx.TotalFiles
	completedFiles := ctx.CompletedFiles
	failedFiles := ctx.FailedFiles
	isProcessed := ctx.Processed
	ctx.mu.RUnlock()

	// Log transfer state
	log.Debug("transfers").
		Int64("id", transferID).
		Int32("completed_files", completedFiles).
		Int32("failed_files", failedFiles).
		Int32("total_files", totalFiles).
		Bool("is_processed", isProcessed == Processed).
		Msg("Transfer completion status")

	// Check if all files are completed and the transfer is not yet processed
	if completedFiles+failedFiles >= totalFiles && failedFiles == 0 && isProcessed == NotProcessed {
		// Count active files for this transfer
		activeCount := 0
		m.activeFiles.Range(func(key, value interface{}) bool {
			fileTransferID := value.(int64)
			if fileTransferID == transferID {
				activeCount++
			}
			return true
		})

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
			} else {
				// Update the transfer cache with the completed status
				if processor := m.GetTransferProcessor(); processor != nil {
					if processor.transferCache != nil {
						// Update the progress in the cache
						processor.transferCache.UpdateTransferProgress(transferID, ctx.DownloadedSize)
					}
				}
			}
		}
	}
}

// handleFileFailure marks a file as failed in the transfer context
// This is called when a file fails to download
func (m *Manager) handleFileFailure(transferID int64) {
	if err := m.coordinator.FileFailure(transferID); err != nil {
		log.Error("transfers").
			Int64("transfer_id", transferID).
			Err(err).
			Msg("Failed to handle file failure")
	}
}
