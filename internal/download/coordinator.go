package download

import (
	"fmt"
	"sync"

	"github.com/elsbrock/plundrio/internal/log"
)

// TransferCoordinator manages the lifecycle of transfers and their associated downloads
type TransferCoordinator struct {
	transfers           sync.Map // map[int64]*TransferContext
	onTransferProcessed func(int64)
	cleanupHooks        []func(int64) error
}

// NewTransferCoordinator creates a new transfer coordinator.
// onProcessed is called when a transfer reaches the Processed state.
func NewTransferCoordinator(onProcessed func(int64)) *TransferCoordinator {
	return &TransferCoordinator{
		onTransferProcessed: onProcessed,
		cleanupHooks:        make([]func(int64) error, 0),
	}
}

// RangeTransfers calls fn for each tracked transfer context.
// If fn returns false, iteration stops.
func (tc *TransferCoordinator) RangeTransfers(fn func(transferID int64, ctx *TransferContext) bool) {
	tc.transfers.Range(func(key, value interface{}) bool {
		return fn(key.(int64), value.(*TransferContext))
	})
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
		state:      TransferLifecycleInitial,
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

	if ctx.state != TransferLifecycleInitial {
		return fmt.Errorf("invalid state transition: %s -> Downloading", ctx.state)
	}

	ctx.state = TransferLifecycleDownloading
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
	if ctx.state == TransferLifecycleCompleted {
		return nil
	}

	// Allow file completions even if the transfer is in a failed state
	// This lets us track progress even if some files failed
	if ctx.state != TransferLifecycleDownloading && ctx.state != TransferLifecycleFailed {
		return fmt.Errorf("cannot complete file: transfer %d is in state %s", transferID, ctx.state)
	}

	ctx.completedFiles++
	completed := ctx.completedFiles

	// Calculate progress based on file count
	fileProgress := float64(completed) / float64(ctx.TotalFiles) * 100

	// Calculate progress based on bytes if we have size information
	var bytesProgress float64
	if ctx.totalSize > 0 {
		bytesProgress = float64(ctx.downloadedSize) / float64(ctx.totalSize) * 100
	}

	log.Info("transfer").
		Int64("id", transferID).
		Str("name", ctx.Name).
		Int32("completed", completed).
		Int32("total", ctx.TotalFiles).
		Float64("file_progress", fileProgress).
		Int64("downloaded_bytes", ctx.downloadedSize).
		Int64("total_bytes", ctx.totalSize).
		Float64("bytes_progress", bytesProgress).
		Msg("File completed")

	// Check if all files are done (completed + failed = total)
	if completed+ctx.failedFiles >= ctx.TotalFiles {
		// Only mark as completed if there are no failed files
		if ctx.failedFiles == 0 {
			ctx.state = TransferLifecycleCompleted
			log.Info("transfer").
				Int64("id", transferID).
				Str("name", ctx.Name).
				Int32("completed", completed).
				Int32("total", ctx.TotalFiles).
				Int64("downloaded_bytes", ctx.downloadedSize).
				Int64("total_bytes", ctx.totalSize).
				Msg("Transfer marked as completed, waiting for final cleanup")
		} else {
			ctx.state = TransferLifecycleFailed
			log.Info("transfer").
				Int64("id", transferID).
				Str("name", ctx.Name).
				Int32("failed", ctx.failedFiles).
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
	if ctx.state == TransferLifecycleCompleted {
		return nil
	}

	// Increment failed files counter
	ctx.failedFiles++
	failed := ctx.failedFiles
	completed := ctx.completedFiles
	total := ctx.TotalFiles

	// Mark transfer as failed but don't delete it
	ctx.state = TransferLifecycleFailed

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
	if ctx.state != TransferLifecycleDownloading && ctx.state != TransferLifecycleCompleted {
		return fmt.Errorf("invalid state transition: %s -> Completed", ctx.state)
	}

	// Make sure it's marked as completed (might already be)
	ctx.state = TransferLifecycleCompleted

	// Double-check that all files are actually completed
	if ctx.completedFiles+ctx.failedFiles < ctx.TotalFiles {
		log.Warn("transfer").
			Int64("id", transferID).
			Str("name", ctx.Name).
			Int32("completed", ctx.completedFiles).
			Int32("failed", ctx.failedFiles).
			Int32("total", ctx.TotalFiles).
			Msg("Attempting to complete transfer before all files are done")
		return fmt.Errorf("cannot complete transfer: %d/%d files still pending",
			ctx.TotalFiles-(ctx.completedFiles+ctx.failedFiles), ctx.TotalFiles)
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
	ctx.state = TransferLifecycleProcessed

	// Notify that the transfer has been processed
	tc.onTransferProcessed(transferID)

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
		ctx.state = TransferLifecycleCancelled
		ctx.err = err
		log.Info("transfer").
			Int64("id", transferID).
			Str("name", ctx.Name).
			Msg("Transfer cancelled")
		return nil
	}

	// For real failures, mark as failed but don't clean up
	ctx.state = TransferLifecycleFailed
	ctx.err = err

	log.Error("transfer").
		Int64("id", transferID).
		Str("name", ctx.Name).
		Err(err).
		Msg("Transfer failed but keeping context for retry")

	return nil
}

// GetTransferContext safely retrieves a transfer context
func (tc *TransferCoordinator) GetTransferContext(transferID int64) (*TransferContext, bool) {
	if value, ok := tc.transfers.Load(transferID); ok {
		return value.(*TransferContext), true
	}
	log.Debug("transfer").
		Int64("id", transferID).
		Msg("Transfer context not found in coordinator")

	return nil, false
}
