package download

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/putdotio/go-putio/putio"
)

// downloadWorker processes download jobs from the queue
func (m *Manager) downloadWorker() {
	defer m.wg.Done()

	for {
		select {
		case <-m.stopChan:
			return
		case job := <-m.jobs:
			if job.IsFolder {
				m.handleFolder(job)
			} else {
				state := &DownloadState{
					FileID: job.FileID,
					Name:   job.Name,
					Status: "downloading",
				}
				if err := m.downloadFile(state); err != nil {
					log.Printf("Failed to download %s: %v", job.Name, err)
				}
			}
		}
	}
}

// handleFolder processes a folder and queues its contents for download
func (m *Manager) handleFolder(job downloadJob) {
	// Get folder contents
	files, err := m.client.GetFiles(job.FileID)
	if err != nil {
		log.Printf("Failed to get folder contents for %s: %v", job.Name, err)
		return
	}

	// Create the folder
	folderPath := filepath.Join(m.cfg.TargetDir, job.Name)
	if err := os.MkdirAll(folderPath, 0755); err != nil {
		log.Printf("Failed to create folder %s: %v", folderPath, err)
		return
	}

	// Queue all files and subfolders
	for _, file := range files {
		subPath := filepath.Join(job.Name, file.Name)
		m.QueueDownload(downloadJob{
			FileID:   file.ID,
			Name:     subPath,
			IsFolder: file.IsDir(),
		})
	}
}

// setupDownloadContext creates a context for the download that's cancelled when stopChan is closed
func (m *Manager) setupDownloadContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-m.stopChan:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// setupTransferCleanup sets up cleanup for the download state and transfer tracking
func (m *Manager) setupTransferCleanup(state *DownloadState) func() {
	return func() {
		m.activeFiles.Delete(state.FileID)

		// Clean up the active downloads tracking and handle transfer completion
		m.transferMutex.Lock()
		defer m.transferMutex.Unlock()

		if transferID := state.TransferID; transferID > 0 {
			m.transferFiles[transferID]--
			if m.transferFiles[transferID] <= 0 {
				log.Printf("All files for '%s' have been downloaded", state.Name)

				// Get transfers to determine status
				transfers, err := m.client.GetTransfers()
				if err != nil {
					log.Printf("Failed to get transfer status: %v", err)
					return
				}

				// Find our transfer
				var targetTransfer *putio.Transfer
				for _, t := range transfers {
					if t.ID == transferID {
						targetTransfer = t
						break
					}
				}

				if targetTransfer == nil {
					log.Printf("Transfer for '%s' no longer exists on Put.io", state.Name)
					return
				}

				// Verify all files exist locally before cleanup
				files, err := m.client.GetAllTransferFiles(targetTransfer.FileID)
				if err != nil {
					log.Printf("Failed to get files for transfer %d: %v", transferID, err)
					return
				}

				// Check each file exists locally with matching size
				allFilesExist := true
				for _, file := range files {
					localPath := filepath.Join(m.cfg.TargetDir, file.Name)
					info, err := os.Stat(localPath)
					if err != nil {
						if os.IsNotExist(err) {
							allFilesExist = false
							break
						}
						log.Printf("Failed to check local file %s: %v", localPath, err)
						return
					}

					if info.Size() != file.Size {
						allFilesExist = false
						break
					}
				}

				if allFilesExist {
					shouldDelete := targetTransfer.Status == "COMPLETED" ||
						(targetTransfer.Status == "SEEDING" && m.cfg.DeleteBeforeCompleted)

					if shouldDelete {
						log.Printf("All files exist locally for '%s', cleaning up from Put.io (status: %s)", state.Name, targetTransfer.Status)
						if err := m.client.DeleteFile(targetTransfer.FileID); err != nil {
							log.Printf("Failed to delete '%s' from Put.io: %v", state.Name, err)
						}
						if err := m.client.DeleteTransfer(transferID); err != nil {
							log.Printf("Failed to delete transfer for '%s': %v", state.Name, err)
						}
					} else {
						log.Printf("Keeping '%s' on Put.io for seeding", state.Name)
					}
				} else {
					log.Printf("Some files for '%s' are missing or have incorrect sizes, keeping on Put.io", state.Name)
				}
			}
		}
	}
}

// downloadFile downloads a file from Put.io to the target directory
func (m *Manager) downloadFile(state *DownloadState) error {
	ctx, cancel := m.setupDownloadContext()
	defer cancel()
	defer m.setupTransferCleanup(state)()

	url, err := m.client.GetDownloadURL(state.FileID)
	if err != nil {
		return fmt.Errorf("failed to get download URL: %w", err)
	}

	targetPath, tempPath, tempFile, startOffset, err := m.prepareDownloadFile(state)
	if err != nil {
		return err
	}
	defer func() {
		tempFile.Close()
		if err != nil {
			os.Remove(tempPath)
		}
	}()

	log.Println("Downloading", url)
	req, err := m.createDownloadRequest(ctx, url, startOffset)
	if err != nil {
		return err
	}

	client := m.createHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to start download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("download failed: %s", resp.Status)
	}

	// Set up progress tracking
	totalSize := resp.ContentLength
	if totalSize <= 0 {
		return nil // No download of empty files
	}

	if startOffset > 0 && resp.StatusCode == http.StatusOK {
		// Server doesn't support range requests, start over
		log.Printf("Server doesn't support range requests, starting download from beginning for %s", state.Name)
		startOffset = 0
		// Truncate the file since we're starting over
		if err := tempFile.Truncate(0); err != nil {
			return fmt.Errorf("failed to truncate file: %w", err)
		}
		if _, err := tempFile.Seek(0, 0); err != nil {
			return fmt.Errorf("failed to seek to start: %w", err)
		}
	} else if resp.StatusCode == http.StatusPartialContent {
		// For partial content, add the existing bytes to total size
		totalSize += startOffset
	}

	downloaded := startOffset // Start with existing bytes for progress calculation

	// Create progress logging ticker
	progressTicker := time.NewTicker(progressUpdateInterval)
	defer progressTicker.Stop()

	// Create done channel for progress goroutine
	done := make(chan struct{})
	defer close(done)

	reader := m.setupProgressTracking(state, resp.Body, &downloaded, totalSize)

	m.monitorDownloadProgress(ctx, state, reader, totalSize, &downloaded, done, progressTicker)
	m.monitorDownloadStall(ctx, state, &downloaded, totalSize, cancel)

	// Create a pipe to allow cancellation of io.Copy
	pr, pw := io.Pipe()
	copyDone := make(chan error, 1)

	// Start copying in a goroutine
	go func() {
		_, err := io.Copy(tempFile, reader)
		if err != nil {
			log.Printf("Copy error for %s: %v", state.Name, err)
			copyDone <- err
			return
		}
		copyDone <- nil
	}()

	// Wait for completion or cancellation
	select {
	case err = <-copyDone:
		if err != nil {
			return fmt.Errorf("download failed: %w", err)
		}
		return m.finalizeDownload(state, reader, tempFile, tempPath, targetPath, totalSize)
	case <-ctx.Done():
		// Clean up on cancellation
		pr.Close()
		pw.Close()
		tempFile.Close()
		os.Remove(tempPath)
		return fmt.Errorf("download cancelled")
	}
}
