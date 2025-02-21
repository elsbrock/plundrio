package download

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/putdotio/go-putio/putio"
)

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

	log.Printf("Cleaned up completed transfer '%s'", downloadState.Name)
	return nil
}

// FindIncompleteDownloads checks for any incomplete downloads in the target directory
func (m *Manager) FindIncompleteDownloads() ([]downloadJob, error) {
	files, err := m.client.GetFiles(m.cfg.FolderID)
	if err != nil {
		return nil, fmt.Errorf("failed to get folder contents: %w", err)
	}

	var incompleteJobs []downloadJob
	for _, file := range files {
		tempPath := filepath.Join(m.cfg.TargetDir, fmt.Sprintf("download-%d.tmp", file.ID))
		if _, err := os.Stat(tempPath); err == nil {
			incompleteJobs = append(incompleteJobs, downloadJob{
				FileID:   file.ID,
				Name:     file.Name,
				IsFolder: file.IsDir(),
			})
			log.Printf("Found incomplete download: %s", file.Name)
		}
	}

	return incompleteJobs, nil
}

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

	// Map of transfer IDs to their current status
	currentStatus := make(map[int64]string)

	for _, t := range transfers {
		// Skip transfers not in our target folder
		if t.SaveParentID != m.cfg.FolderID {
			continue
		}
		currentStatus[t.ID] = t.Status
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
		// Check if we're already handling it
		if _, exists := m.active.Load(transfer.ID); exists {
			continue
		}

		log.Printf("Found ready transfer '%s' (status: %s)", transfer.Name, transfer.Status)

		// Trigger download
		m.workerWg.Add(1)
		go m.processTransfer(transfer)
	}

	// Display errored transfers, then delete
	for _, transfer := range erroredTransfers {
		log.Printf("Transfer '%s' errored: %s", transfer.Name, transfer.ErrorMessage)
		if err := m.client.DeleteTransfer(transfer.ID); err != nil {
			log.Printf("Failed to delete errored transfer: %v", err)
		}
	}
}

// handleFileCompletion updates transfer state when a file completes downloading
func (m *Manager) handleFileCompletion(transferID int64) {
	m.transferMutex.Lock()
	defer m.transferMutex.Unlock()

	tstate, exists := m.transferStates[transferID]
	if !exists {
		return
	}

	tstate.completedFiles++
	log.Printf("Completed %d/%d files for transfer %d", tstate.completedFiles, tstate.totalFiles, transferID)

	// If all files are done, clean up the transfer
	if tstate.completedFiles >= tstate.totalFiles {
		if err := m.cleanupTransfer(transferID); err != nil {
			log.Printf("Failed to cleanup completed transfer: %v", err)
		}
	}
}

// formatETA formats the estimated time remaining in a human-readable format
func formatETA(seconds int) string {
	if seconds <= 0 {
		return "unknown"
	}
	duration := time.Duration(seconds) * time.Second
	hours := int(duration.Hours())
	minutes := int(duration.Minutes()) % 60
	secs := int(duration.Seconds()) % 60

	if hours > 0 {
		return fmt.Sprintf("%dh%02dm", hours, minutes)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm%02ds", minutes, secs)
	}
	return fmt.Sprintf("%ds", secs)
}

// processTransfer handles downloading of a completed or seeding transfer
func (m *Manager) processTransfer(transfer *putio.Transfer) {
	defer m.workerWg.Done()

	// Store transfer state
	state := &DownloadState{
		TransferID: transfer.ID,
		FileID:     transfer.FileID,
		Name:       transfer.Name,
	}
	m.active.Store(transfer.ID, state)

	// Get all files in the transfer
	files, err := m.client.GetAllTransferFiles(transfer.FileID)
	if err != nil {
		if putioErr, ok := err.(*putio.ErrorResponse); ok && putioErr.Type == "NotFound" {
			log.Printf("Files of transfer '%s' no longer exists on Put.io, cleaning up", transfer.Name)
			_ = m.client.DeleteTransfer(transfer.ID)
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
	} else {
		// No files to download, clean up immediately
		if err := m.cleanupTransfer(transfer.ID); err != nil {
			log.Printf("Failed to cleanup transfer: %v", err)
		}
	}
}
