package download

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/elsbrock/plundrio/internal/log"
)

// TransferCoordinator manages the lifecycle of transfers and their associated downloads
type TransferCoordinator struct {
	transfers    sync.Map // map[int64]*TransferContext
	manager      *Manager
	cleanupHooks []func(int64) error
}

// NewTransferCoordinator creates a new transfer coordinator
func NewTransferCoordinator(manager *Manager) *TransferCoordinator {
	return &TransferCoordinator{
		manager:      manager,
		cleanupHooks: make([]func(int64) error, 0),
	}
}

// RegisterCleanupHook adds a function to be called during transfer cleanup
func (tc *TransferCoordinator) RegisterCleanupHook(hook func(int64) error) {
	tc.cleanupHooks = append(tc.cleanupHooks, hook)
}

// InitiateTransfer starts tracking a new transfer
func (tc *TransferCoordinator) InitiateTransfer(id int64, name string, fileID int64, totalFiles int) *TransferContext {
	ctx := &TransferContext{
		ID:         id,
		Name:       name,
		FileID:     fileID,
		TotalFiles: int32(totalFiles),
		State:      TransferLifecycleInitial,
	}
	tc.transfers.Store(id, ctx)

	log.Info("transfer").
		Int64("id", id).
		Str("name", name).
		Int("total_files", totalFiles).
		Msg("Initiated new transfer")

	return ctx
}

// StartDownload marks a transfer as downloading
func (tc *TransferCoordinator) StartDownload(transferID int64) error {
	ctx, ok := tc.GetTransferContext(transferID)
	if !ok {
		return NewTransferNotFoundError(transferID)
	}

	ctx.mu.Lock()
	defer ctx.mu.Unlock()

	if ctx.State != TransferLifecycleInitial {
		return fmt.Errorf("invalid state transition: %s -> Downloading", ctx.State)
	}

	ctx.State = TransferLifecycleDownloading
	log.Info("transfer").
		Int64("id", transferID).
		Str("name", ctx.Name).
		Msg("Started transfer download")

	return nil
}

// FileCompleted marks a file as completed and checks if the transfer is done
func (tc *TransferCoordinator) FileCompleted(transferID int64) error {
	ctx, ok := tc.GetTransferContext(transferID)
	if !ok {
		// If transfer not found, it might have been already completed
		log.Debug("transfer").
			Int64("id", transferID).
			Msg("Transfer not found during file completion, might be already completed")
		return nil
	}

	ctx.mu.Lock()
	defer ctx.mu.Unlock()

	// If transfer is already completed, just return
	if ctx.State == TransferLifecycleCompleted {
		return nil
	}

	// Allow file completions even if the transfer is in a failed state
	// This lets us track progress even if some files failed
	if ctx.State != TransferLifecycleDownloading && ctx.State != TransferLifecycleFailed {
		return fmt.Errorf("cannot complete file: transfer %d is in state %s", transferID, ctx.State)
	}

	completed := atomic.AddInt32(&ctx.CompletedFiles, 1)

	// Calculate progress based on file count
	fileProgress := float64(completed) / float64(ctx.TotalFiles) * 100

	// Calculate progress based on bytes if we have size information
	var bytesProgress float64
	if ctx.TotalSize > 0 {
		bytesProgress = float64(ctx.DownloadedSize) / float64(ctx.TotalSize) * 100
	}

	log.Info("transfer").
		Int64("id", transferID).
		Str("name", ctx.Name).
		Int32("completed", completed).
		Int32("total", ctx.TotalFiles).
		Float64("file_progress", fileProgress).
		Int64("downloaded_bytes", ctx.DownloadedSize).
		Int64("total_bytes", ctx.TotalSize).
		Float64("bytes_progress", bytesProgress).
		Msg("File completed")

	// Check if all files are done (completed + failed = total)
	if completed+ctx.FailedFiles >= ctx.TotalFiles {
		// Only mark as completed if there are no failed files
		if ctx.FailedFiles == 0 {
			// We already have the lock, so just update the state
			// BUT don't remove the transfer context yet - active downloads may still need it
			ctx.State = TransferLifecycleCompleted
			log.Info("transfer").
				Int64("id", transferID).
				Str("name", ctx.Name).
				Int32("completed", completed).
				Int32("total", ctx.TotalFiles).
				Int64("downloaded_bytes", ctx.DownloadedSize).
				Int64("total_bytes", ctx.TotalSize).
				Msg("Transfer marked as completed, waiting for final cleanup")

			// The actual cleanup and transfer context removal will happen
			// when all downloads have explicitly finished and CompleteTransfer is called
		} else {
			// If there are failed files, mark as failed but don't delete
			ctx.State = TransferLifecycleFailed
			log.Info("transfer").
				Int64("id", transferID).
				Str("name", ctx.Name).
				Int32("failed", ctx.FailedFiles).
				Int32("total", ctx.TotalFiles).
				Msg("Transfer has failed files, keeping for retry")
		}
	}

	return nil
}

// FileFailure marks a file as failed but keeps the transfer context
func (tc *TransferCoordinator) FileFailure(transferID int64) error {
	ctx, ok := tc.GetTransferContext(transferID)
	if !ok {
		// If transfer not found, it might have been already completed
		log.Debug("transfer").
			Int64("id", transferID).
			Msg("Transfer not found during file failure, might be already completed")
		return nil
	}

	ctx.mu.Lock()
	defer ctx.mu.Unlock()

	// If transfer is already completed, just return
	if ctx.State == TransferLifecycleCompleted {
		return nil
	}

	// Increment failed files counter
	failed := atomic.AddInt32(&ctx.FailedFiles, 1)
	completed := ctx.CompletedFiles
	total := ctx.TotalFiles

	// Mark transfer as failed but don't delete it
	ctx.State = TransferLifecycleFailed

	log.Error("transfer").
		Int64("id", transferID).
		Str("name", ctx.Name).
		Int32("failed", failed).
		Int32("completed", completed).
		Int32("total", total).
		Msg("File failed but keeping transfer for retry")

	// Check if all files are processed (completed + failed = total)
	if completed+failed >= total {
		log.Info("transfer").
			Int64("id", transferID).
			Str("name", ctx.Name).
			Int32("failed", failed).
			Int32("completed", completed).
			Int32("total", total).
			Msg("All files processed, some failed, keeping transfer for retry")
	}

	return nil
}

// CompleteTransfer marks a transfer as completed and triggers cleanup
// This now marks the transfer as processed instead of removing it
func (tc *TransferCoordinator) CompleteTransfer(transferID int64) error {
	ctx, ok := tc.GetTransferContext(transferID)
	if !ok {
		return NewTransferNotFoundError(transferID)
	}

	ctx.mu.Lock()
	defer ctx.mu.Unlock()

	// Allow completion from both Downloading and Completed states
	// This handles both:
	// 1. Normal completion directly from Downloading
	// 2. Final cleanup for transfers already marked Completed but waiting for active downloads
	if ctx.State != TransferLifecycleDownloading && ctx.State != TransferLifecycleCompleted {
		return fmt.Errorf("invalid state transition: %s -> Completed", ctx.State)
	}

	// Make sure it's marked as completed (might already be)
	ctx.State = TransferLifecycleCompleted

	// Double-check that all files are actually completed
	if ctx.CompletedFiles+ctx.FailedFiles < ctx.TotalFiles {
		log.Warn("transfer").
			Int64("id", transferID).
			Str("name", ctx.Name).
			Int32("completed", ctx.CompletedFiles).
			Int32("failed", ctx.FailedFiles).
			Int32("total", ctx.TotalFiles).
			Msg("Attempting to complete transfer before all files are done")
		return fmt.Errorf("cannot complete transfer: %d/%d files still pending",
			ctx.TotalFiles-(ctx.CompletedFiles+ctx.FailedFiles), ctx.TotalFiles)
	}

	log.Info("transfer").
		Int64("id", transferID).
		Str("name", ctx.Name).
		Msg("Transfer fully completed and cleaning up")

	// Run cleanup hooks
	for _, hook := range tc.cleanupHooks {
		if err := hook(transferID); err != nil {
			log.Error("transfer").
				Int64("id", transferID).
				Err(err).
				Msg("Cleanup hook failed")
		}
	}

	// Mark the transfer as processed instead of removing it
	ctx.State = TransferLifecycleProcessed

	// Mark the transfer as processed in the processor
	tc.manager.GetTransferProcessor().MarkTransferProcessed(transferID)

	log.Info("transfer").
		Int64("id", transferID).
		Str("name", ctx.Name).
		Msg("Transfer processed")

	return nil
}

// FailTransfer marks a transfer as failed
func (tc *TransferCoordinator) FailTransfer(transferID int64, err error) error {
	ctx, ok := tc.GetTransferContext(transferID)
	if !ok {
		return NewTransferNotFoundError(transferID)
	}

	ctx.mu.Lock()
	defer ctx.mu.Unlock()

	// Check if this is a cancellation
	if downloadErr, ok := err.(*DownloadError); ok && downloadErr.Type == "DownloadCancelled" {
		// For cancellations, just mark as cancelled but keep the transfer
		ctx.State = TransferLifecycleCancelled
		ctx.Error = err
		log.Info("transfer").
			Int64("id", transferID).
			Str("name", ctx.Name).
			Msg("Transfer cancelled")
		return nil
	}

	// For real failures, mark as failed but don't clean up
	// We'll keep the transfer context so we can retry later
	ctx.State = TransferLifecycleFailed
	ctx.Error = err

	log.Error("transfer").
		Int64("id", transferID).
		Str("name", ctx.Name).
		Err(err).
		Msg("Transfer failed but keeping context for retry")

	// Don't run cleanup hooks or delete the transfer context
	// This allows other files to continue downloading and we can retry failed files later
	return nil
}

// GetTransferContext safely retrieves a transfer context
func (tc *TransferCoordinator) GetTransferContext(transferID int64) (*TransferContext, bool) {
	if value, ok := tc.transfers.Load(transferID); ok {
		return value.(*TransferContext), true
	}
	// Add debug logging when transfer context is not found
	log.Debug("transfer").
		Int64("id", transferID).
		Msg("Transfer context not found in coordinator")

	return nil, false
}
