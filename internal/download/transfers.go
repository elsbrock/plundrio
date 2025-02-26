package download

import (
	"os"
	"path/filepath"
	"time"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/log"
)

// TransferProcessor handles the processing of Put.io transfers
type TransferProcessor struct {
	manager   *Manager
	transfers map[string][]*putio.Transfer // Status -> Transfers
	folderID  int64
	targetDir string
}

// newTransferProcessor creates a new transfer processor
func newTransferProcessor(m *Manager) *TransferProcessor {
	return &TransferProcessor{
		manager:   m,
		transfers: make(map[string][]*putio.Transfer),
		folderID:  m.cfg.FolderID,
		targetDir: m.cfg.TargetDir,
	}
}

// monitorTransfers periodically checks for completed transfers
func (m *Manager) monitorTransfers() {
	log.Debug("transfers").Msg("Starting transfer monitor")
	processor := newTransferProcessor(m)

	// Initial check
	processor.checkTransfers()

	ticker := time.NewTicker(m.dlConfig.TransferCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopChan:
			log.Debug("transfers").Msg("Transfer monitor stopping")
			return
		case <-ticker.C:
			processor.checkTransfers()
		}
	}
}

// checkTransfers looks for completed or seeding transfers and processes them
func (p *TransferProcessor) checkTransfers() {
	log.Debug("transfers").Msg("Checking transfers")

	transfers, err := p.manager.client.GetTransfers()
	if err != nil {
		log.Error("transfers").Err(err).Msg("Failed to get transfers")
		return
	}

	// Reset transfer status tracking
	p.transfers = make(map[string][]*putio.Transfer)

	// Categorize transfers by status
	for _, t := range transfers {
		if t.SaveParentID != p.folderID {
			log.Debug("transfers").
				Int64("transfer_id", t.ID).
				Int64("parent_id", t.SaveParentID).
				Int64("target_folder", p.folderID).
				Msg("Skipping transfer from different folder")
			continue
		}
		p.transfers[t.Status] = append(p.transfers[t.Status], t)
	}

	// Log transfer summary
	p.logTransferSummary()

	// Process transfers by status
	p.processInProgressTransfers()
	p.processReadyTransfers()
	p.processErroredTransfers()
}

// logTransferSummary logs counts of transfers in each status
func (p *TransferProcessor) logTransferSummary() {
	counts := map[string]int{
		"IN_QUEUE":    len(p.transfers["IN_QUEUE"]),
		"WAITING":     len(p.transfers["WAITING"]),
		"PREPARING":   len(p.transfers["PREPARING"]),
		"DOWNLOADING": len(p.transfers["DOWNLOADING"]),
		"COMPLETING":  len(p.transfers["COMPLETING"]),
		"SEEDING":     len(p.transfers["SEEDING"]),
		"COMPLETED":   len(p.transfers["COMPLETED"]),
		"ERROR":       len(p.transfers["ERROR"]),
	}

	log.Info("transfers").
		Int("queued", counts["IN_QUEUE"]).
		Int("waiting", counts["WAITING"]).
		Int("preparing", counts["PREPARING"]).
		Int("downloading", counts["DOWNLOADING"]).
		Int("completing", counts["COMPLETING"]).
		Int("seeding", counts["SEEDING"]).
		Int("completed", counts["COMPLETED"]).
		Int("error", counts["ERROR"]).
		Msg("Transfer status summary")
}

// processInProgressTransfers logs details for transfers being downloaded
func (p *TransferProcessor) processInProgressTransfers() {
	for _, t := range p.transfers["DOWNLOADING"] {
		log.Info("transfers").
			Str("name", t.Name).
			Float64("progress_percent", float64(t.PercentDone)).
			Float64("size_mb", float64(t.Size)/(1024*1024)).
			Float64("speed_mbps", float64(t.DownloadSpeed)/(1024*1024)).
			Str("eta", formatETA(int(t.EstimatedTime))).
			Msg("Transfer in progress")
	}
}

// processReadyTransfers handles completed and seeding transfers
func (p *TransferProcessor) processReadyTransfers() {
	readyTransfers := append(p.transfers["COMPLETED"], p.transfers["SEEDING"]...)

	for _, transfer := range readyTransfers {
		select {
		case <-p.manager.stopChan:
			log.Debug("transfers").Msg("Stopping transfer processing")
			return
		default:
			if p.isTransferBeingProcessed(transfer.ID) {
				continue
			}
			p.startTransferProcessing(transfer)
		}
	}
}

// isTransferBeingProcessed checks if a transfer is already being handled
func (p *TransferProcessor) isTransferBeingProcessed(transferID int64) bool {
	if _, exists := p.manager.coordinator.getTransferContext(transferID); exists {
		log.Debug("transfers").
			Int64("transfer_id", transferID).
			Msg("Transfer already being processed")
		return true
	}
	return false
}

// startTransferProcessing begins processing a transfer
func (p *TransferProcessor) startTransferProcessing(transfer *putio.Transfer) {
	log.Info("transfers").
		Str("name", transfer.Name).
		Str("status", transfer.Status).
		Int64("id", transfer.ID).
		Msg("Found ready transfer")

	p.manager.workerWg.Add(1)
	transferCopy := *transfer
	go func() {
		p.processTransfer(&transferCopy)
	}()
}

// processTransfer handles downloading of a completed or seeding transfer
func (p *TransferProcessor) processTransfer(transfer *putio.Transfer) {
	defer p.manager.workerWg.Done()

	log.Debug("transfers").
		Str("name", transfer.Name).
		Int64("id", transfer.ID).
		Int64("file_id", transfer.FileID).
		Msg("Processing transfer")

	files, err := p.manager.client.GetAllTransferFiles(transfer.FileID)
	if err != nil {
		p.handleTransferError(transfer, err)
		return
	}

	if len(files) == 0 {
		err := NewNoFilesFoundError(transfer.ID)
		p.manager.coordinator.FailTransfer(transfer.ID, err)
		return
	}

	// Initialize transfer with total number of files
	if !p.initializeTransfer(transfer, len(files)) {
		return
	}

	// Queue files that need downloading
	filesToDownload := p.queueTransferFiles(transfer, files)

	// If no files need downloading (all exist), complete the transfer
	if filesToDownload == 0 {
		log.Info("transfers").
			Str("name", transfer.Name).
			Int64("id", transfer.ID).
			Msg("All files already exist, completing transfer")
		p.manager.coordinator.CompleteTransfer(transfer.ID)
		return
	}
}

// handleTransferError processes transfer errors appropriately
func (p *TransferProcessor) handleTransferError(transfer *putio.Transfer, err error) {
	if putioErr, ok := err.(*putio.ErrorResponse); ok && putioErr.Type == "NotFound" {
		log.Info("transfers").
			Str("name", transfer.Name).
			Int64("id", transfer.ID).
			Msg("Files no longer exist on Put.io, cleaning up")
		p.manager.cleanupTransfer(transfer.ID)
		return
	}

	log.Error("transfers").
		Str("name", transfer.Name).
		Int64("id", transfer.ID).
		Err(err).
		Msg("Failed to get transfer files")
}

// queueTransferFiles processes files in a transfer and queues them for download
func (p *TransferProcessor) queueTransferFiles(transfer *putio.Transfer, files []*putio.File) int {
	filesToDownload := 0
	for _, file := range files {
		if p.shouldDownloadFile(transfer, file) {
			filesToDownload++
			p.queueFileDownload(transfer, file)
		}
	}
	return filesToDownload
}

// shouldDownloadFile determines if a file needs to be downloaded
func (p *TransferProcessor) shouldDownloadFile(transfer *putio.Transfer, file *putio.File) bool {
	targetPath := filepath.Join(p.targetDir, transfer.Name, file.Name)
	info, err := os.Stat(targetPath)

	// Skip if file exists with correct size
	if err == nil && info.Size() == file.Size {
		log.Info("transfers").
			Str("file_name", file.Name).
			Int64("file_id", file.ID).
			Msg("File already exists, skipping download")
		return false
	}

	// Skip if already being downloaded
	if _, exists := p.manager.activeFiles.Load(file.ID); exists {
		log.Debug("transfers").
			Str("file_name", file.Name).
			Int64("file_id", file.ID).
			Msg("File already being downloaded")
		return false
	}

	return true
}

// queueFileDownload adds a file to the download queue
func (p *TransferProcessor) queueFileDownload(transfer *putio.Transfer, file *putio.File) {
	p.manager.QueueDownload(downloadJob{
		FileID:     file.ID,
		Name:       filepath.Join(transfer.Name, file.Name),
		TransferID: transfer.ID,
	})
	log.Debug("transfers").
		Str("file_name", file.Name).
		Int64("file_id", file.ID).
		Int64("size", file.Size).
		Msg("Queued file for download")
}

// initializeTransfer sets up transfer tracking
func (p *TransferProcessor) initializeTransfer(transfer *putio.Transfer, filesToDownload int) bool {
	p.manager.coordinator.InitiateTransfer(transfer.ID, transfer.Name, transfer.FileID, filesToDownload)
	if err := p.manager.coordinator.StartDownload(transfer.ID); err != nil {
		log.Error("transfers").
			Str("name", transfer.Name).
			Int64("id", transfer.ID).
			Err(err).
			Msg("Failed to start transfer download")
		p.manager.coordinator.FailTransfer(transfer.ID, err)
		return false
	}

	if filesToDownload > 0 {
		log.Info("transfers").
			Str("name", transfer.Name).
			Int("files", filesToDownload).
			Msg("Queued files for download")
	}
	return true
}

// processErroredTransfers handles failed transfers
func (p *TransferProcessor) processErroredTransfers() {
	for _, transfer := range p.transfers["ERROR"] {
		log.Error("transfers").
			Str("name", transfer.Name).
			Int64("id", transfer.ID).
			Str("error", transfer.ErrorMessage).
			Msg("Transfer errored")

		if err := p.manager.client.DeleteTransfer(transfer.ID); err != nil {
			log.Error("transfers").
				Str("name", transfer.Name).
				Int64("id", transfer.ID).
				Err(err).
				Msg("Failed to delete errored transfer")
		}
	}
}
