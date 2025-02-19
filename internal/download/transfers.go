package download

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/putdotio/go-putio/putio"
)

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
	defer m.wg.Done()

	m.checkTransfers() // Initial check
	ticker := time.NewTicker(30 * time.Second)
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
	var idle, downloading, seeding, completed, error int
	var inProgressTransfers []*putio.Transfer

	// Map of transfer IDs to their current status
	currentStatus := make(map[int64]string)

	for _, t := range transfers {
		if t.SaveParentID != m.cfg.FolderID {
			continue // Skip transfers not in our target folder
		}
		currentStatus[t.ID] = t.Status
		switch t.Status {
		case "IN_QUEUE":
			idle++
		case "DOWNLOADING":
			downloading++
			inProgressTransfers = append(inProgressTransfers, t)
		case "SEEDING":
			seeding++
		case "COMPLETED":
			completed++
		case "ERROR":
			error++
		}
	}

	// Log transfer counts
	log.Printf("Transfers - Idle: %d, Downloading: %d, Seeding: %d, Completed: %d, Error: %d",
		idle, downloading, seeding, completed, error)

	// Log details for in-progress transfers
	for _, t := range inProgressTransfers {
		log.Printf("Downloading: %s (%.1f%% of %.1fMB at %.1fMB/s, ETA: %s)",
			t.Name,
			float64(t.PercentDone),
			float64(t.Size)/(1024*1024),
			float64(t.DownloadSpeed)/(1024*1024),
			formatETA(int(t.EstimatedTime)))
	}

	// Check for transfers that were seeding and are now completed
	m.transferMutex.Lock()
	for transferID := range m.completedTransfers {
		// If we were keeping it for seeding and it's now completed, clean it up
		if status, exists := currentStatus[transferID]; exists && status == "COMPLETED" {
			// Find the transfer object
			var transfer *putio.Transfer
			for _, t := range transfers {
				if t.ID == transferID {
					transfer = t
					break
				}
			}
			if transfer != nil {
				log.Printf("Transfer '%s' finished seeding, cleaning up", transfer.Name)
				if err := m.client.DeleteFile(transfer.FileID); err != nil {
					log.Printf("Failed to delete '%s' from Put.io: %v", transfer.Name, err)
				}
				if err := m.client.DeleteTransfer(transfer.ID); err != nil {
					log.Printf("Failed to delete transfer for '%s': %v", transfer.Name, err)
				}
				delete(m.completedTransfers, transferID)
			}
		}
	}
	m.transferMutex.Unlock()

	// Process new transfers
	for _, transfer := range transfers {
		// Skip transfers not in our target folder or already being processed
		if transfer.SaveParentID != m.cfg.FolderID {
			continue
		}
		if _, exists := m.active.Load(transfer.ID); exists {
			continue
		}

		log.Printf("Found transfer '%s' (status: %s, folder: %d)",
			transfer.Name, transfer.Status, transfer.SaveParentID)
		switch transfer.Status {
		case "COMPLETED", "SEEDING":
			m.wg.Add(1)
			go m.processTransfer(transfer)
		case "ERROR":
			log.Printf("Transfer error: %s - %s", transfer.Name, transfer.ErrorMessage)
			if err := m.client.DeleteTransfer(transfer.ID); err != nil {
				log.Printf("Failed to delete failed transfer: %v", err)
			}
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
	defer m.wg.Done()

	state := &DownloadState{
		TransferID: transfer.ID,
		FileID:     transfer.FileID,
		Name:       transfer.Name,
		Status:     "downloading",
	}
	m.active.Store(transfer.ID, state)
	defer m.active.Delete(transfer.ID)

	// Get all files in the transfer
	files, err := m.client.GetAllTransferFiles(transfer.FileID)
	if err != nil {
		if putioErr, ok := err.(*putio.ErrorResponse); ok && putioErr.Type == "NotFound" {
			log.Printf("Transfer '%s' no longer exists on Put.io, cleaning up", transfer.Name)
			_ = m.client.DeleteTransfer(transfer.ID)
			state.Status = "error"
			return
		}
		log.Printf("Failed to get transfer files: %v", err)
		state.Status = "error"
		return
	}

	// Store transfer name for path construction
	m.transferMutex.Lock()
	m.transferNames[transfer.ID] = transfer.Name
	m.transferMutex.Unlock()

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
			log.Printf("Queueing download for %s", file.Name)
			// Queue download for this file
			m.QueueDownload(downloadJob{
				FileID:     file.ID,
				Name:       filepath.Join(transfer.Name, file.Name),
				TransferID: transfer.ID,
			})
		}
	}

	// Update transfer files count for tracking
	m.transferMutex.Lock()
	if filesToDownload > 0 {
		log.Printf("Queued %d files for download from transfer '%s'", filesToDownload, transfer.Name)
		m.transferFiles[transfer.ID] = filesToDownload
	} else {
		log.Printf("All files already exist locally for transfer '%s', cleaning up", transfer.Name)
		// Clean up immediately since no downloads are needed
		if transfer.Status == "COMPLETED" || (transfer.Status == "SEEDING" && m.cfg.DeleteBeforeCompleted) {
			log.Printf("Cleaning up '%s' from Put.io (status: %s)", transfer.Name, transfer.Status)
			if err := m.client.DeleteFile(transfer.FileID); err != nil {
				log.Printf("Failed to delete '%s' from Put.io: %v", transfer.Name, err)
			}
			if err := m.client.DeleteTransfer(transfer.ID); err != nil {
				log.Printf("Failed to delete transfer for '%s': %v", transfer.Name, err)
			}
		} else {
			log.Printf("Keeping '%s' on Put.io for seeding", transfer.Name)
		}
	}
	m.transferMutex.Unlock()

	// Let setupTransferCleanup handle the cleanup after all downloads complete
	state.Status = "downloading"
}
