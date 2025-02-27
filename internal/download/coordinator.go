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
	ctx, ok := tc.getTransferContext(transferID)
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
	ctx, ok := tc.getTransferContext(transferID)
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

	if ctx.State != TransferLifecycleDownloading {
		return fmt.Errorf("cannot complete file: transfer %d is in state %s", transferID, ctx.State)
	}

	completed := atomic.AddInt32(&ctx.CompletedFiles, 1)
	progress := float64(completed) / float64(ctx.TotalFiles) * 100

	log.Info("transfer").
		Int64("id", transferID).
		Str("name", ctx.Name).
		Int32("completed", completed).
		Int32("total", ctx.TotalFiles).
		Float64("progress", progress).
		Msg("File completed")

	// Check if all files are done
	if completed >= ctx.TotalFiles {
		// We already have the lock, so call complete directly
		ctx.State = TransferLifecycleCompleted
		log.Info("transfer").
			Int64("id", transferID).
			Str("name", ctx.Name).
			Msg("Transfer completed")

		// Run cleanup hooks
		for _, hook := range tc.cleanupHooks {
			if err := hook(transferID); err != nil {
				log.Error("transfer").
					Int64("id", transferID).
					Err(err).
					Msg("Cleanup hook failed")
			}
		}

		// Remove transfer context
		tc.transfers.Delete(transferID)
	}

	return nil
}

// CompleteTransfer marks a transfer as completed and triggers cleanup
func (tc *TransferCoordinator) CompleteTransfer(transferID int64) error {
	ctx, ok := tc.getTransferContext(transferID)
	if !ok {
		return NewTransferNotFoundError(transferID)
	}

	ctx.mu.Lock()
	defer ctx.mu.Unlock()

	if ctx.State != TransferLifecycleDownloading {
		return fmt.Errorf("invalid state transition: %s -> Completed", ctx.State)
	}

	ctx.State = TransferLifecycleCompleted
	log.Info("transfer").
		Int64("id", transferID).
		Str("name", ctx.Name).
		Msg("Transfer completed")

	// Run cleanup hooks
	for _, hook := range tc.cleanupHooks {
		if err := hook(transferID); err != nil {
			log.Error("transfer").
				Int64("id", transferID).
				Err(err).
				Msg("Cleanup hook failed")
		}
	}

	// Remove transfer context
	tc.transfers.Delete(transferID)
	return nil
}

// FailTransfer marks a transfer as failed
func (tc *TransferCoordinator) FailTransfer(transferID int64, err error) error {
	ctx, ok := tc.getTransferContext(transferID)
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

	// For real failures, mark as failed and clean up
	ctx.State = TransferLifecycleFailed
	ctx.Error = err

	log.Error("transfer").
		Int64("id", transferID).
		Str("name", ctx.Name).
		Err(err).
		Msg("Transfer failed")

	// Run cleanup hooks for permanent failures
	for _, hook := range tc.cleanupHooks {
		if err := hook(transferID); err != nil {
			log.Error("transfer").
				Int64("id", transferID).
				Err(err).
				Msg("Cleanup hook failed")
		}
	}

	// Remove transfer context for permanent failures
	tc.transfers.Delete(transferID)
	return nil
}

// getTransferContext safely retrieves a transfer context
func (tc *TransferCoordinator) getTransferContext(transferID int64) (*TransferContext, bool) {
	if value, ok := tc.transfers.Load(transferID); ok {
		return value.(*TransferContext), true
	}
	// Add debug logging when transfer context is not found
	log.Debug("transfer").
		Int64("id", transferID).
		Msg("Transfer context not found in coordinator")

	// Debug: List all known transfers
	var knownTransfers []int64
	tc.transfers.Range(func(key, value interface{}) bool {
		knownTransfers = append(knownTransfers, key.(int64))
		return true
	})
	log.Debug("transfer").
		Interface("known_transfers", knownTransfers).
		Msg("Currently tracked transfers")

	return nil, false
}
