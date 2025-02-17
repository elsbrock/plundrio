// Package download provides functionality for managing downloads from Put.io.
// It handles concurrent downloads, progress tracking, and cleanup of completed transfers.
package download

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/elsbrock/plundrio/internal/api"
	"github.com/elsbrock/plundrio/internal/config"
	"github.com/putdotio/go-putio/putio"
)

const (
	defaultWorkerCount       = 4
	progressUpdateInterval   = 5 * time.Second
	downloadStallTimeout    = 1 * time.Minute
	downloadHeaderTimeout   = 30 * time.Second
	idleConnectionTimeout   = 60 * time.Second
	downloadBufferMultiple  = 2 // Buffer size multiplier for download jobs channel
)

// ErrDownloadStalled is returned when a download makes no progress for too long
type ErrDownloadStalled struct {
	Filename string
	Duration time.Duration
}

func (e *ErrDownloadStalled) Error() string {
	return fmt.Sprintf("download stalled for %v: %s", e.Duration, e.Filename)
}

// downloadJob represents a single download task
type downloadJob struct {
	FileID     int64
	Name       string
	IsFolder   bool
	TransferID int64 // Parent transfer ID for group tracking
}

// Manager handles downloading completed transfers from Put.io.
// It supports concurrent downloads, progress tracking, and automatic cleanup
// of completed transfers. The manager uses a worker pool pattern to process
// downloads efficiently while maintaining control over system resources.
type Manager struct {
	cfg         *config.Config
	client      *api.Client
	active      sync.Map // map[int64]*DownloadState
	activeFiles sync.Map // map[int64]bool - tracks files being downloaded
	stopChan    chan struct{}
	wg          sync.WaitGroup
	jobs        chan downloadJob
	mu          sync.Mutex // protects job queueing

	// Transfer tracking
	transferFiles     map[int64]int // Track total files per transfer
	completedTransfers map[int64]bool // Track completed transfers
	transferMutex    sync.Mutex
}

// QueueDownload adds a download job to the queue if not already downloading
func (m *Manager) QueueDownload(job downloadJob) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if file is already being downloaded
	if _, exists := m.activeFiles.Load(job.FileID); exists {
		return
	}

	// Mark file as being downloaded before queueing
	m.activeFiles.Store(job.FileID, true)
	select {
	case m.jobs <- job:
		// Successfully queued
	case <-m.stopChan:
		// Manager is shutting down, remove from active files
		m.activeFiles.Delete(job.FileID)
	}
}

// FindIncompleteDownloads checks for any incomplete downloads in the target directory
func (m *Manager) FindIncompleteDownloads() ([]downloadJob, error) {
	// Get all files in our target folder
	files, err := m.client.GetFiles(m.cfg.FolderID)
	if err != nil {
		return nil, fmt.Errorf("failed to get folder contents: %w", err)
	}

	var incompleteJobs []downloadJob
	for _, file := range files {
		// Check if we have a partial download
		tempPath := filepath.Join(m.cfg.TargetDir, fmt.Sprintf("download-%d.tmp", file.ID))
		if _, err := os.Stat(tempPath); err == nil {
			// Found a partial download
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

// DownloadState tracks the progress of a file download
type DownloadState struct {
	TransferID    int64
	FileID        int64
	Name          string
	Status        string
	Progress      float64
	ETA           time.Time
	LastProgress  time.Time
	StartTime     time.Time
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
		stopChan:          make(chan struct{}),
		jobs:              make(chan downloadJob, workerCount*downloadBufferMultiple),
		activeFiles:       sync.Map{},
		transferFiles:     make(map[int64]int),
		completedTransfers: make(map[int64]bool),
	}

	return m
}

// Start begins monitoring transfers and downloading completed ones
func (m *Manager) Start() {
	workerCount := m.cfg.WorkerCount
	if workerCount <= 0 {
		workerCount = 4
	}

	// Start download workers
	for i := 0; i < workerCount; i++ {
		m.wg.Add(1)
		go m.downloadWorker()
	}

	// Start transfer monitor
	m.wg.Add(1)
	go m.monitorTransfers()
}

// Stop gracefully shuts down the manager
func (m *Manager) Stop() {
	close(m.stopChan)
	m.wg.Wait()
}

// monitorTransfers periodically checks for completed transfers
func (m *Manager) monitorTransfers() {
	defer m.wg.Done()

	// tick immediately on start
	m.checkTransfers()

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

// checkTransfers looks for completed transfers and starts downloads
func (m *Manager) checkTransfers() {
	transfers, err := m.client.GetTransfers()
	if err != nil {
		log.Printf("Failed to get transfers: %v", err)
		return
	}

	// Count transfers in each status
	var idle, downloading, seeding, completed, error int
	for _, t := range transfers {
		if t.SaveParentID != m.cfg.FolderID {
			continue // Skip transfers not in our target folder
		}
		switch t.Status {
		case "IN_QUEUE":
			idle++
		case "DOWNLOADING":
			downloading++
		case "SEEDING":
			seeding++
		case "COMPLETED":
			completed++
		case "ERROR":
			error++
		}
	}
	log.Printf("Transfers - Idle: %d, Downloading: %d, Seeding: %d, Completed: %d, Error: %d",
		idle, downloading, seeding, completed, error)

	// Get files in our target folder to check seeding status and reconcile files
	files, err := m.client.GetFiles(m.cfg.FolderID)
	if err != nil {
		log.Printf("Failed to get folder contents: %v", err)
		return
	}

	// Create a map of transfer names to files for matching
	filesByName := make(map[string]*putio.File)
	for _, file := range files {
		filesByName[file.Name] = file

		// For folders, we need to process them even if they exist locally
		// to ensure all contents are synced
		if file.IsDir() {
			m.QueueDownload(downloadJob{
				FileID:   file.ID,
				Name:     file.Name,
				IsFolder: true,
			})
		} else {
			// For regular files, check if they exist locally
			targetPath := filepath.Join(m.cfg.TargetDir, file.Name)
			_, err := os.Stat(targetPath)
			if os.IsNotExist(err) {
				m.QueueDownload(downloadJob{
					FileID:   file.ID,
					Name:     file.Name,
					IsFolder: false,
				})
			} else if err != nil {
				log.Printf("Error checking local file %s: %v", file.Name, err)
			} else if m.cfg.EarlyFileDelete {
				if err := m.client.DeleteFile(file.ID); err != nil {
					log.Printf("Failed to delete existing file: %v", err)
				}
			}
		}
	}

	for _, transfer := range transfers {
		// Skip transfers we're already tracking
		if _, exists := m.active.Load(transfer.ID); exists {
			continue
		}

		// Only process transfers in our target folder
		if transfer.SaveParentID != m.cfg.FolderID {
			continue
		}

		switch transfer.Status {
		case "COMPLETED":
			// Start download
			m.wg.Add(1)
			go m.handleCompletedTransfer(transfer)
		case "SEEDING":
			// Check if we've downloaded this file
			if file := filesByName[transfer.Name]; file != nil {
				targetPath := filepath.Join(m.cfg.TargetDir, file.Name)
				if _, err := os.Stat(targetPath); err == nil {
					// File exists locally and is seeding, clean it up
					log.Printf("Cleaning up seeded file: %s", file.Name)
					if err := m.client.DeleteFile(file.ID); err != nil {
						log.Printf("Failed to delete seeded file: %v", err)
					}
					if err := m.client.DeleteTransfer(transfer.ID); err != nil {
						log.Printf("Failed to delete seeded transfer: %v", err)
					}
				}
			}
		case "ERROR":
			log.Printf("Transfer error: %s - %s", transfer.Name, transfer.ErrorMessage)
			// Clean up failed transfer
			if err := m.client.DeleteTransfer(transfer.ID); err != nil {
				log.Printf("Failed to delete failed transfer: %v", err)
			}
		}
	}

	// We've already handled file reconciliation above, no need to check again
}

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

// handleCompletedTransfer processes a completed transfer
func (m *Manager) handleCompletedTransfer(transfer *putio.Transfer) {
	defer m.wg.Done()

	state := &DownloadState{
		TransferID: transfer.ID,
		FileID:     transfer.FileID,
		Name:       transfer.Name,
		Status:     "downloading",
	}
	m.active.Store(transfer.ID, state)

	// Get file info to check if it's a folder
	file, err := m.client.GetFile(transfer.FileID)
	if err != nil {
		log.Printf("Failed to get file info: %v", err)
		state.Status = "error"
		return
	}

	// Count files in transfer and initialize tracking
	m.transferMutex.Lock()
	if file.IsDir() {
		// For folders, get all files recursively
		files, err := m.client.GetFiles(file.ID)
		if err != nil {
			log.Printf("Failed to get folder contents: %v", err)
			m.transferMutex.Unlock()
			state.Status = "error"
			return
		}
		// Count all files (not folders) in the transfer
		fileCount := 0
		var countFiles func(files []*putio.File)
		countFiles = func(files []*putio.File) {
			for _, f := range files {
				if f.IsDir() {
					subFiles, err := m.client.GetFiles(f.ID)
					if err != nil {
						log.Printf("Failed to get subfolder contents: %v", err)
						continue
					}
					countFiles(subFiles)
				} else {
					fileCount++
				}
			}
		}
		countFiles(files)
		m.transferFiles[transfer.ID] = fileCount
	} else {
		// Single file transfer
		m.transferFiles[transfer.ID] = 1
	}
	m.transferMutex.Unlock()

	// Queue the initial download job
	m.QueueDownload(downloadJob{
		FileID:     file.ID,
		Name:       file.Name,
		IsFolder:   file.IsDir(),
		TransferID: transfer.ID,
	})

	// Clean up the transfer from Put.io
	if err := m.client.DeleteTransfer(transfer.ID); err != nil {
		log.Printf("Failed to delete transfer: %v", err)
	}

	state.Status = "completed"
	m.active.Delete(transfer.ID)
}

// downloadFile downloads a file from Put.io to the target directory
func (m *Manager) downloadFile(state *DownloadState) error {
	// Create a context that's cancelled when stopChan is closed
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-m.stopChan:
			cancel()
		case <-ctx.Done():
		}
	}()
	defer cancel()
	// Clean up activeFiles tracking when done
	defer func() {
		m.activeFiles.Delete(state.FileID)

		// Check if this was the last file in the transfer
		m.transferMutex.Lock()
		defer m.transferMutex.Unlock()
		if transferID := state.TransferID; transferID > 0 {
			m.transferFiles[transferID]--
			if m.transferFiles[transferID] <= 0 && !m.completedTransfers[transferID] {
				log.Printf("All files downloaded for transfer %d, deleting file %d", transferID, state.FileID)
				if err := m.client.DeleteFile(state.FileID); err == nil {
					m.completedTransfers[transferID] = true
				} else {
					log.Printf("Failed to delete file %d: %v", state.FileID, err)
				}
			}
		}
	}()

	// Get download URL
	url, err := m.client.GetDownloadURL(state.FileID)
	if err != nil {
		return fmt.Errorf("failed to get download URL: %w", err)
	}

	// Create target directory
	targetPath := filepath.Join(m.cfg.TargetDir, state.Name)
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Check if partial file exists
	tempPath := filepath.Join(filepath.Dir(targetPath), fmt.Sprintf("download-%d.tmp", state.FileID))
	var tempFile *os.File
	var startOffset int64

	// Try to open existing temp file
	if existingFile, err := os.OpenFile(tempPath, os.O_RDWR, 0644); err == nil {
		// Get size of existing file
		if info, err := existingFile.Stat(); err == nil {
			startOffset = info.Size()
			tempFile = existingFile
			log.Printf("Resuming download of %s from offset %d", state.Name, startOffset)
		} else {
			existingFile.Close()
		}
	}

	// Create new temp file if we couldn't use existing one
	if tempFile == nil {
		tempFile, err = os.OpenFile(tempPath, os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			return fmt.Errorf("failed to create temporary file: %w", err)
		}
	}

	defer func() {
		tempFile.Close()
		if err != nil {
			// Clean up temp file if there was an error
			os.Remove(tempPath)
		}
	}()

	// Create request with Range header if resuming
	log.Println("Downloading", url)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Add User-Agent header
	req.Header.Set("User-Agent", "plundrio/1.0")

	if startOffset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", startOffset))
	}

	// Add common headers that might help
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Connection", "keep-alive")

	// Create custom HTTP client with longer timeouts and keep-alive
	client := &http.Client{
		Timeout: 0, // No timeout for large downloads
		Transport: &http.Transport{
			DisableCompression: true,  // Disable compression for large files
			DisableKeepAlives: false,  // Enable keep-alives
			IdleConnTimeout: 60 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}

	// Download file
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

	startTime := time.Now()
	reader := &progressReader{
		reader:     resp.Body,
		startTime:  startTime,
		onProgress: func(n int64) {
			downloaded += n
			if totalSize > 0 {
				state.Progress = float64(downloaded) / float64(totalSize)

				// Calculate ETA based on current download rate
				elapsed := time.Since(startTime).Seconds()
				if elapsed > 0 {
					speed := float64(downloaded) / elapsed
					remaining := float64(totalSize - downloaded)
					if speed > 0 { // Avoid division by zero
						etaSeconds := remaining / speed
						state.ETA = time.Now().Add(time.Duration(etaSeconds) * time.Second)
					}
				}
			}
			state.LastProgress = time.Now()
		},
	}

	// Start progress logging goroutine
	go func() {
		log.Printf("Starting download of %s (%.2f MB)", state.Name, float64(totalSize)/1024/1024)
		for {
			select {
			case <-progressTicker.C:
				if totalSize > 0 {
					progress := float64(downloaded) / float64(totalSize) * 100
					downloadedMB := float64(downloaded) / 1024 / 1024
					totalMB := float64(totalSize) / 1024 / 1024
					elapsed := time.Since(reader.startTime).Seconds()
					speedMBps := downloadedMB / elapsed
					eta := state.ETA.Sub(time.Now()).Round(time.Second)
					log.Printf("Downloading %s: %.1f%% (%.1f/%.1f MB) - %.2f MB/s ETA: %s",
						state.Name, progress, downloadedMB, totalMB, speedMBps, eta)
				}
			case <-ctx.Done():
				log.Printf("Download of %s cancelled", state.Name)
				return
			case <-done:
				return
			}
		}
	}()

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

	// Add a timeout to detect stalled downloads
	downloadTimeout := time.NewTimer(30 * time.Second)
	defer downloadTimeout.Stop()

	lastProgress := downloaded
	lastProgressTime := time.Now()
	go func() {
		for {
			select {
			case <-time.After(5 * time.Second):
				currentDownloaded := downloaded
				if currentDownloaded == lastProgress && currentDownloaded < totalSize {
					stalledDuration := time.Since(lastProgressTime)
					if stalledDuration > 1*time.Minute {
						log.Printf("Download %s stalled for over 1 minute, cancelling", state.Name)
						cancel()
						return
					}
					log.Printf("Warning: Download %s stalled for %v", state.Name, stalledDuration.Round(time.Second))
				} else {
					lastProgress = currentDownloaded
					lastProgressTime = time.Now()
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Wait for either completion or cancellation
	select {
	case err = <-copyDone:
		if err != nil {
			return fmt.Errorf("download failed: %w", err)
		}
		// Ensure all data is written to disk
		if err = tempFile.Sync(); err != nil {
			return fmt.Errorf("failed to sync file: %w", err)
		}
	case <-ctx.Done():
		// Clean up on cancellation
		pr.Close()
		pw.Close()
		tempFile.Close()
		os.Remove(tempPath)
		return fmt.Errorf("download cancelled")
	}

	// Close the temp file before moving it
	tempFile.Close()

	// Move temp file to target path
	if err = os.Rename(tempPath, targetPath); err != nil {
		return fmt.Errorf("failed to move file to target location: %w", err)
	}

	// File deletion is now handled at transfer level after all files complete

	elapsed := time.Since(reader.startTime).Seconds()
	averageSpeedMBps := (float64(totalSize) / 1024 / 1024) / elapsed
	log.Printf("Completed download of %s (%.2f MB) - Average speed: %.2f MB/s",
		state.Name, float64(totalSize)/1024/1024, averageSpeedMBps)
	return nil
}

type progressReader struct {
	reader     io.Reader
	onProgress func(n int64)
	startTime  time.Time
}

func (r *progressReader) Read(p []byte) (n int, err error) {
	n, err = r.reader.Read(p)
	if n > 0 && r.onProgress != nil {
		r.onProgress(int64(n))
	}
	return
}
