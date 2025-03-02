package download

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/log"
)

// TransferProcessor handles the processing of Put.io transfers
type TransferProcessor struct {
	manager            *Manager
	transfers          map[string][]*putio.Transfer // Status -> Transfers
	processedTransfers sync.Map                     // map[int64]bool - Tracks transfers that have been processed locally
	retryAttempts      sync.Map                     // map[int64]int - Tracks retry attempts for errored transfers
	folderID           int64
	targetDir          string
	transferCache      *TransferCacheImpl   // Cache for transfer information
	progressTracker    *ProgressTrackerImpl // Tracker for progress calculation
}

// GetTransfers returns a copy of all transfers for a given folder ID
func (p *TransferProcessor) GetTransfers() []*putio.Transfer {
	var allTransfers []*putio.Transfer
	for _, transfers := range p.transfers {
		for _, t := range transfers {
			if t.SaveParentID == p.folderID {
				allTransfers = append(allTransfers, t)
			}
		}
	}
	return allTransfers
}

// newTransferProcessor creates a new transfer processor
func newTransferProcessor(m *Manager) *TransferProcessor {
	processor := &TransferProcessor{
		manager:            m,
		transfers:          make(map[string][]*putio.Transfer),
		processedTransfers: sync.Map{},
		retryAttempts:      sync.Map{},
		folderID:           m.cfg.FolderID,
		targetDir:          m.cfg.TargetDir,
	}

	// Create progress tracker
	processor.progressTracker = NewProgressTracker(processor, m.coordinator, m.dlConfig)

	// Create transfer cache
	processor.transferCache = NewTransferCache(m.client, m.dlConfig, processor.progressTracker)

	return processor
}

// monitorTransfers periodically checks for completed transfers
func (m *Manager) monitorTransfers() {
	log.Debug("transfers").Msg("Starting transfer monitor")
	processor := newTransferProcessor(m)

	// Store the processor in the manager for access by other components
	m.processor = processor

	log.Debug("transfers").
		Int64("folder_id", processor.folderID).
		Str("target_dir", processor.targetDir).
		Msg("Transfer processor initialized")

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

	// Update the transfer cache
	if err := p.transferCache.UpdateCache(); err != nil {
		log.Error("transfers").Err(err).Msg("Failed to update transfer cache")
		return
	}

	// Get transfers from the API
	transfers, err := p.manager.client.GetTransfers()
	if err != nil {
		log.Error("transfers").Err(err).Msg("Failed to get transfers")
		return
	}

	log.Debug("transfers").
		Int("api_transfers_count", len(transfers)).
		Msg("Retrieved transfers from API")

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
	p.processReadyTransfers()
	p.processErroredTransfers()

	// Check for transfers that have all files completed but haven't been fully processed
	p.finalizeCompletedTransfers()
}

// logTransferSummary logs counts of transfers in each status and detailed information for all transfers
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

	// Log detailed information for all transfers
	p.logAllTransfersDetails()
}

// logAllTransfersDetails logs detailed information for all transfers
func (p *TransferProcessor) logAllTransfersDetails() {
	allTransfers := p.GetTransfers()
	if len(allTransfers) == 0 {
		log.Debug("transfers").Msg("No transfers found for detailed logging")
		return
	}

	for _, t := range allTransfers {
		// Create a logger with common fields for all transfers
		transferLogger := log.Info("transfers").
			Int64("id", t.ID).
			Str("name", t.Name).
			Str("status", t.Status).
			Int64("save_parent_id", t.SaveParentID).
			Int64("file_id", t.FileID).
			Int("size", t.Size).
			Str("type", t.Type).
			Str("status_message", t.StatusMessage).
			Int("availability", t.Availability).
			Bool("is_private", t.IsPrivate)

		// Add peer information if available
		if t.PeersConnected > 0 || t.PeersSendingToUs > 0 || t.PeersGettingFromUs > 0 {
			transferLogger = transferLogger.
				Int("peers_connected", t.PeersConnected).
				Int("peers_sending_to_us", t.PeersSendingToUs).
				Int("peers_getting_from_us", t.PeersGettingFromUs)
		}

		// Add speed information if available
		if t.DownloadSpeed > 0 || t.UploadSpeed > 0 {
			transferLogger = transferLogger.
				Int("download_speed", t.DownloadSpeed).
				Int("upload_speed", t.UploadSpeed).
				Int64("downloaded", t.Downloaded).
				Int64("uploaded", t.Uploaded)
		}

		// Add progress information if available
		if t.PercentDone > 0 && t.PercentDone < 100 {
			transferLogger = transferLogger.
				Int("percent_done", t.PercentDone).
				Int64("estimated_time", t.EstimatedTime)
		}

		// Add seeding information if available
		if t.SecondsSeeding > 0 {
			transferLogger = transferLogger.Int("seconds_seeding", t.SecondsSeeding)
		}

		// Add tracker information if available
		if t.Trackers != "" {
			transferLogger = transferLogger.Str("trackers", t.Trackers)
		}
		if t.TrackerMessage != "" {
			transferLogger = transferLogger.Str("tracker_message", t.TrackerMessage)
		}

		// Add torrent information if available
		if t.MagnetURI != "" {
			transferLogger = transferLogger.Str("magnet_uri", t.MagnetURI)
		}
		if t.TorrentLink != "" {
			transferLogger = transferLogger.Str("torrent_link", t.TorrentLink)
		}
		if t.CreatedTorrent {
			transferLogger = transferLogger.Bool("created_torrent", t.CreatedTorrent)
		}

		// Add error information if available
		if t.ErrorMessage != "" {
			transferLogger = transferLogger.Str("error_message", t.ErrorMessage)
		}

		// Add timestamps if available
		if t.CreatedAt != nil {
			transferLogger = transferLogger.Interface("created_at", t.CreatedAt)
		}
		if t.FinishedAt != nil {
			transferLogger = transferLogger.Interface("finished_at", t.FinishedAt)
		}

		// Add other miscellaneous fields
		if t.ClientIP != "" {
			transferLogger = transferLogger.Str("client_ip", t.ClientIP)
		}
		if t.CallbackURL != "" {
			transferLogger = transferLogger.Str("callback_url", t.CallbackURL)
		}
		if t.Extract {
			transferLogger = transferLogger.Bool("extract", t.Extract)
		}
		if t.DownloadID != 0 {
			transferLogger = transferLogger.Int64("download_id", t.DownloadID)
		}
		if t.SubscriptionID != 0 {
			transferLogger = transferLogger.Int("subscription_id", t.SubscriptionID)
		}

		// Check if this transfer is being processed locally
		_, processed := p.processedTransfers.Load(t.ID)
		transferLogger = transferLogger.Bool("processed_locally", processed)

		// Log the transfer details with a message that includes the status
		transferLogger.Msgf("Transfer details (%s)", t.Status)
	}
}

// processReadyTransfers handles completed and seeding transfers
func (p *TransferProcessor) processReadyTransfers() {
	// Process completed transfers
	for _, transfer := range p.transfers["COMPLETED"] {
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

	// Process seeding transfers
	for _, transfer := range p.transfers["SEEDING"] {
		select {
		case <-p.manager.stopChan:
			log.Debug("transfers").Msg("Stopping transfer processing")
			return
		default:
			// Check if the transfer has seeded long enough
			if p.shouldCancelSeedingTransfer(transfer) {
				p.cancelSeedingTransfer(transfer)
				continue
			}

			// If not being processed and not seeded long enough, process it
			if !p.isTransferBeingProcessed(transfer.ID) {
				p.startTransferProcessing(transfer)
			}
		}
	}
}

// shouldCancelSeedingTransfer checks if a seeding transfer has seeded long enough
func (p *TransferProcessor) shouldCancelSeedingTransfer(transfer *putio.Transfer) bool {
	// If the transfer has no seeding time, don't cancel it
	if transfer.SecondsSeeding <= 0 {
		return false
	}

	// Convert seconds to duration
	seedingDuration := time.Duration(transfer.SecondsSeeding) * time.Second

	// Check if it has seeded longer than the threshold
	if seedingDuration >= p.manager.dlConfig.SeedingTimeThreshold {
		log.Info("transfers").
			Str("name", transfer.Name).
			Int64("id", transfer.ID).
			Int("seconds_seeding", transfer.SecondsSeeding).
			Str("threshold", p.manager.dlConfig.SeedingTimeThreshold.String()).
			Msg("Transfer has seeded long enough, will be cancelled")
		return true
	}

	log.Debug("transfers").
		Str("name", transfer.Name).
		Int64("id", transfer.ID).
		Int("seconds_seeding", transfer.SecondsSeeding).
		Str("threshold", p.manager.dlConfig.SeedingTimeThreshold.String()).
		Msg("Transfer still seeding, not yet reached threshold")

	return false
}

// cancelSeedingTransfer cancels a seeding transfer
func (p *TransferProcessor) cancelSeedingTransfer(transfer *putio.Transfer) {
	log.Info("transfers").
		Str("name", transfer.Name).
		Int64("id", transfer.ID).
		Int("seconds_seeding", transfer.SecondsSeeding).
		Msg("Cancelling seeding transfer that has seeded long enough")

	// Cancel the transfer
	if err := p.manager.client.DeleteTransfer(transfer.ID); err != nil {
		log.Error("transfers").
			Str("name", transfer.Name).
			Int64("id", transfer.ID).
			Err(err).
			Msg("Failed to cancel seeding transfer")
		return
	}

	// Remove the transfer from the cache if it has a hash
	if transfer.Hash != "" {
		p.transferCache.RemoveTransfer(transfer.Hash)
	}

	log.Info("transfers").
		Str("name", transfer.Name).
		Int64("id", transfer.ID).
		Msg("Successfully cancelled seeding transfer")
}

// isTransferBeingProcessed checks if a transfer is already being handled
func (p *TransferProcessor) isTransferBeingProcessed(transferID int64) bool {
	if _, exists := p.manager.coordinator.GetTransferContext(transferID); exists {
		log.Debug("transfers").
			Int64("transfer_id", transferID).
			Msg("Transfer already being processed")
		return true
	}
	return false
}

// startTransferProcessing begins processing a transfer
func (p *TransferProcessor) startTransferProcessing(transfer *putio.Transfer) {
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
		Str("status", transfer.Status).
		Msg("Processing transfer")

	// Special handling for seeding transfers with no files
	if transfer.Status == "SEEDING" && !transfer.UserfileExists {
		// For seeding transfers without files, check if it has seeded long enough
		if p.shouldCancelSeedingTransfer(transfer) {
			p.cancelSeedingTransfer(transfer)
		} else {
			log.Debug("transfers").
				Str("name", transfer.Name).
				Int64("id", transfer.ID).
				Msg("Seeding transfer with no files, leaving for next check")
		}
		return
	}

	// For transfers with no files, mark as completed
	if !transfer.UserfileExists {
		log.Debug("transfers").
			Str("name", transfer.Name).
			Int64("id", transfer.ID).
			Msg("No associated files, skipping transfer and marking as completed")
		p.manager.coordinator.CompleteTransfer(transfer.ID)
		return
	}

	log.Info("transfers").
		Str("name", transfer.Name).
		Str("status", transfer.Status).
		Int64("id", transfer.ID).
		Msg("Found ready transfer with files")

	files, err := p.manager.client.GetAllTransferFiles(transfer.FileID)
	if err != nil {
		p.handleTransferError(transfer, err)
		return
	}

	if len(files) == 0 {
		// For seeding transfers with no files found, check if it has seeded long enough
		if transfer.Status == "SEEDING" {
			if p.shouldCancelSeedingTransfer(transfer) {
				p.cancelSeedingTransfer(transfer)
				return
			}
			log.Debug("transfers").
				Str("name", transfer.Name).
				Int64("id", transfer.ID).
				Msg("Seeding transfer with no files found, leaving for next check")
			return
		}

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
		// For seeding transfers with all files already downloaded, check if it has seeded long enough
		if transfer.Status == "SEEDING" {
			if p.shouldCancelSeedingTransfer(transfer) {
				p.cancelSeedingTransfer(transfer)
				return
			}
			log.Info("transfers").
				Str("name", transfer.Name).
				Int64("id", transfer.ID).
				Msg("All files already exist for seeding transfer, leaving for next check")
			p.manager.coordinator.CompleteTransfer(transfer.ID)
			return
		}

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
		log.Debug("transfers").
			Str("name", transfer.Name).
			Int64("id", transfer.ID).
			Msg("Files no longer exist on Put.io, cleaning up")

		// Initialize transfer context before cleanup
		p.initializeTransfer(transfer, 0)
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

	// Get the transfer context to update total size
	ctx, exists := p.manager.coordinator.GetTransferContext(transfer.ID)
	if !exists {
		log.Error("transfers").
			Int64("transfer_id", transfer.ID).
			Msg("Transfer context not found when queueing files")
		return 0
	}

	// Calculate total size of all files
	var totalSize int64
	for _, file := range files {
		totalSize += file.Size
	}

	// Update the transfer context with total size
	ctx.mu.Lock()
	ctx.TotalSize = totalSize
	ctx.mu.Unlock()

	// Update the transfer cache with the total size
	if cachedTransfer, ok := p.transferCache.GetTransferByID(transfer.ID); ok {
		cachedTransfer.Size = totalSize
	}

	log.Info("transfers").
		Int64("transfer_id", transfer.ID).
		Int64("total_size", totalSize).
		Int("file_count", len(files)).
		Msg("Updated transfer with total file size")

	for _, file := range files {
		if p.shouldDownloadFile(transfer, file) {
			filesToDownload++
			p.queueFileDownload(transfer, file)
		} else {
			// For files we don't need to download (already exist), mark as completed
			// This ensures our file count tracking is accurate
			if err := p.manager.coordinator.FileCompleted(transfer.ID); err != nil {
				log.Error("transfers").
					Int64("transfer_id", transfer.ID).
					Str("file_name", file.Name).
					Err(err).
					Msg("Failed to mark existing file as completed")
			}

			// For existing files, add their size to the downloaded size
			ctx.mu.Lock()
			ctx.DownloadedSize += file.Size
			ctx.mu.Unlock()

			// Update the transfer cache with the downloaded size
			p.transferCache.UpdateTransferProgress(transfer.ID, ctx.DownloadedSize)

			log.Debug("transfers").
				Int64("transfer_id", transfer.ID).
				Str("file_name", file.Name).
				Int64("file_size", file.Size).
				Msg("Added existing file size to downloaded total")
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

// processErroredTransfers handles failed transfers with retry logic
func (p *TransferProcessor) processErroredTransfers() {
	maxRetryAttempts := p.manager.dlConfig.MaxRetryAttempts

	for _, transfer := range p.transfers["ERROR"] {
		// Get current retry count
		retryCountValue, exists := p.retryAttempts.Load(transfer.ID)
		retryCount := 0
		if exists {
			retryCount = retryCountValue.(int)
		}

		// Log the error with retry information
		logger := log.Error("transfers").
			Str("name", transfer.Name).
			Int64("id", transfer.ID).
			Str("error", transfer.ErrorMessage).
			Int("retry_count", retryCount)

		// Check if we should retry or delete
		if retryCount < maxRetryAttempts {
			// Increment retry count
			p.retryAttempts.Store(transfer.ID, retryCount+1)

			// Log retry attempt
			logger.Msgf("Transfer errored, retrying (attempt %d of %d)", retryCount+1, maxRetryAttempts)

			// Attempt to retry the transfer
			retried, err := p.manager.client.RetryTransfer(transfer.ID)
			if err != nil {
				log.Error("transfers").
					Str("name", transfer.Name).
					Int64("id", transfer.ID).
					Err(err).
					Msgf("Failed to retry transfer (attempt %d of %d)", retryCount+1, maxRetryAttempts)
			} else {
				log.Info("transfers").
					Str("name", transfer.Name).
					Int64("id", transfer.ID).
					Str("new_status", retried.Status).
					Msgf("Successfully retried transfer (attempt %d of %d)", retryCount+1, maxRetryAttempts)

				// Add the retried transfer to the cache
				p.transferCache.AddTransfer(retried)
			}
		} else {
			// Log that we're giving up after max retries
			logger.Msgf("Transfer errored, giving up after %d retry attempts", maxRetryAttempts)

			// Delete the transfer after max retries
			if err := p.manager.client.DeleteTransfer(transfer.ID); err != nil {
				log.Error("transfers").
					Str("name", transfer.Name).
					Int64("id", transfer.ID).
					Err(err).
					Msg("Failed to delete errored transfer after max retries")
			} else {
				// Clear retry counter after successful deletion
				p.retryAttempts.Delete(transfer.ID)

				// Remove the transfer from the cache
				if transfer.Hash != "" {
					p.transferCache.RemoveTransfer(transfer.Hash)
				}

				log.Info("transfers").
					Str("name", transfer.Name).
					Int64("id", transfer.ID).
					Msg("Deleted errored transfer after max retries")
			}
		}
	}
}

// MarkTransferProcessed marks a transfer as processed locally
func (p *TransferProcessor) MarkTransferProcessed(transferID int64) {
	p.processedTransfers.Store(transferID, true)
	log.Debug("transfers").
		Int64("transfer_id", transferID).
		Msg("Marked transfer as processed locally")
}

// finalizeCompletedTransfers checks for transfers that have all files completed
// but haven't been fully processed yet.
func (p *TransferProcessor) finalizeCompletedTransfers() {
	// Get all active transfers from the coordinator
	var pendingCleanup []int64

	p.manager.coordinator.transfers.Range(func(key, value interface{}) bool {
		transferID := key.(int64)
		ctx := value.(*TransferContext)

		// Check if this transfer has all files completed but hasn't been processed
		ctx.mu.RLock()
		isCompletedPending := ctx.Processed == NotProcessed &&
			ctx.CompletedFiles+ctx.FailedFiles >= ctx.TotalFiles &&
			ctx.FailedFiles == 0
		name := ctx.Name
		ctx.mu.RUnlock()

		if isCompletedPending {
			log.Info("transfers").
				Int64("id", transferID).
				Str("name", name).
				Msg("Found completed transfer pending cleanup")
			pendingCleanup = append(pendingCleanup, transferID)
		}
		return true
	})

	// Process any transfers that need cleanup
	for _, transferID := range pendingCleanup {
		log.Info("transfers").
			Int64("id", transferID).
			Msg("Finalizing completed transfer")

		// Call CompleteTransfer which will run cleanup hooks and mark as processed
		if err := p.manager.coordinator.CompleteTransfer(transferID); err != nil {
			log.Error("transfers").
				Int64("id", transferID).
				Err(err).
				Msg("Failed to finalize completed transfer")
		}
	}
}
