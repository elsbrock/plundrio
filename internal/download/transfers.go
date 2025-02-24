package download

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/putdotio/go-putio/putio"
)

// monitorTransfers periodically checks for completed transfers
func (m *Manager) monitorTransfers() {
	m.checkTransfers() // Initial check
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopChan:
			return
		case <-ticker.C:
			m.checkTransfers()
		}
	}
}

// checkTransfers looks for completed or seeding transfers and processes them
func (m *Manager) checkTransfers() {
	transfers, err := m.client.GetTransfers()
	if err != nil {
		log.Printf("Failed to get transfers: %v", err)
		return
	}

	// Count transfers in each status
	var queued, waiting, preparing, downloading, completing, seeding, completed, error int

	var readyTransfers []*putio.Transfer
	var erroredTransfers []*putio.Transfer
	var inProgressTransfers []*putio.Transfer

	for _, t := range transfers {
		// Skip transfers not in our target folder
		if t.SaveParentID != m.cfg.FolderID {
			continue
		}
		switch t.Status {
		case "IN_QUEUE":
			queued++
		case "WAITING":
			waiting++
		case "PREPARING":
			preparing++
		case "DOWNLOADING":
			downloading++
			inProgressTransfers = append(inProgressTransfers, t)
		case "COMPLETING":
			completing++
		case "SEEDING":
			seeding++
			readyTransfers = append(readyTransfers, t)
		case "COMPLETED":
			completed++
			readyTransfers = append(readyTransfers, t)
		case "ERROR":
			error++
			erroredTransfers = append(erroredTransfers, t)
		}
	}

	// Log transfer counts
	log.Printf("Transfers - Queued: %d, Preparing: %d, Downloading: %d, Completing: %d, Seeding: %d, Completed: %d, Error: %d",
		queued, preparing, downloading, completing, seeding, completed, error)

	// Log details for in-progress transfers
	for _, t := range inProgressTransfers {
		log.Printf("- %s (%.1f%% of %.1fMB at %.1fMB/s, ETA: %s)",
			t.Name,
			float64(t.PercentDone),
			float64(t.Size)/(1024*1024),
			float64(t.DownloadSpeed)/(1024*1024),
			formatETA(int(t.EstimatedTime)))
	}

	// Process ready transfers
	for _, transfer := range readyTransfers {
		select {
		case <-m.stopChan:
			return // Exit if shutting down
		default:
			// Check if we're already handling it
			if _, exists := m.active.Load(transfer.ID); exists {
				continue
			}

			log.Printf("Found ready transfer '%s' (status: %s)", transfer.Name, transfer.Status)

			// Safely increment worker count before starting goroutine
			m.workerWg.Add(1)

			// Create a copy of the transfer for the goroutine
			transferCopy := *transfer
			go func() {
				m.processTransfer(&transferCopy)
			}()
		}
	}

	// Display errored transfers, then delete
	for _, transfer := range erroredTransfers {
		log.Printf("Transfer '%s' errored: %s", transfer.Name, transfer.ErrorMessage)
		if err := m.client.DeleteTransfer(transfer.ID); err != nil {
			log.Printf("Failed to delete errored transfer: %v", err)
		}
	}
}

// processTransfer handles downloading of a completed or seeding transfer
func (m *Manager) processTransfer(transfer *putio.Transfer) {
	// Note: We don't defer m.workerWg.Done() here because we need to wait for all
	// queued downloads to complete before decrementing the counter

	// Store transfer state
	m.active.Store(transfer.ID, &DownloadState{
		TransferID: transfer.ID,
		FileID:     transfer.FileID,
		Name:       transfer.Name,
	})

	// Get all files in the transfer
	files, err := m.client.GetAllTransferFiles(transfer.FileID)
	if err != nil {
		defer m.cleanupTransfer(transfer.ID)
		if putioErr, ok := err.(*putio.ErrorResponse); ok && putioErr.Type == "NotFound" {
			log.Printf("Files of transfer '%s' no longer exists on Put.io, cleaning up", transfer.Name)
			return
		}
		log.Printf("Failed to get transfer files: %v", err)
		return
	}

	// Count files that need downloading
	filesToDownload := 0
	for _, file := range files {
		targetPath := filepath.Join(m.cfg.TargetDir, transfer.Name, file.Name)
		info, err := os.Stat(targetPath)

		// File needs downloading if it doesn't exist or size doesn't match
		if err != nil || info.Size() != file.Size {
			// Check if file is already being downloaded
			if _, exists := m.activeFiles.Load(file.ID); exists {
				continue
			}
			filesToDownload++
			// Queue download for this file
			m.QueueDownload(downloadJob{
				FileID:     file.ID,
				Name:       filepath.Join(transfer.Name, file.Name),
				TransferID: transfer.ID,
			})
		} else {
			log.Printf("File '%s' already exists, skipping download", file.Name)
		}
	}

	// Initialize or update transfer state tracking
	m.transferMutex.Lock()
	tstate := &TransferState{
		totalFiles:     filesToDownload,
		completedFiles: 0,
	}
	m.transferStates[transfer.ID] = tstate
	m.transferMutex.Unlock()

	if filesToDownload > 0 {
		log.Printf("Queued %d files for download from transfer '%s'", filesToDownload, transfer.Name)
		// We'll let handleFileCompletion decrement the workerWg when all files are done
	} else {
		// No files to download, clean up immediately and decrement workerWg
		if err := m.cleanupTransfer(transfer.ID); err != nil {
			log.Printf("Failed to cleanup transfer: %v", err)
		}
		// Since no downloads were queued, we can safely decrement the counter here
		m.workerWg.Done()
	}
}

// cleanupTransfer handles the deletion of a completed transfer and its source files
func (m *Manager) cleanupTransfer(transferID int64) error {
	// Get transfer state before cleanup
	state, ok := m.active.Load(transferID)
	if !ok {
		return fmt.Errorf("transfer %d not found", transferID)
	}
	downloadState := state.(*DownloadState)

	// Delete the source file first
	if err := m.client.DeleteFile(downloadState.FileID); err != nil {
		return fmt.Errorf("failed to delete source file: %w", err)
	}

	// Delete the transfer
	if err := m.client.DeleteTransfer(transferID); err != nil {
		return fmt.Errorf("failed to delete transfer: %w", err)
	}

	// Clean up our tracking state
	m.active.Delete(transferID)
	m.transferMutex.Lock()
	delete(m.transferStates, transferID)
	m.transferMutex.Unlock()

	log.Printf("Cleaned up transfer '%s'", downloadState.Name)
	return nil
}

// handleFileCompletion updates transfer state when a file completes downloading
func (m *Manager) handleFileCompletion(transferID int64) {
	state, ok := m.active.Load(transferID)
	if !ok {
		return
	}
	downloadState := state.(*DownloadState)

	m.transferMutex.Lock()
	defer m.transferMutex.Unlock()

	tstate, exists := m.transferStates[transferID]
	if !exists {
		return
	}

	tstate.completedFiles++
	log.Printf("Completed %d/%d files for transfer '%s'", tstate.completedFiles, tstate.totalFiles, downloadState.Name)

	// If all files are done, clean up the transfer and decrement the worker wait group
	if tstate.completedFiles >= tstate.totalFiles {
		if err := m.cleanupTransfer(transferID); err != nil {
			log.Printf("Failed to cleanup completed transfer: %v", err)
		}
		// Now that all downloads for this transfer are complete, we can decrement the worker wait group
		m.workerWg.Done()
	}
}
