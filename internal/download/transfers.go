package download

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/api"
	"github.com/elsbrock/plundrio/internal/log"
)

// transfersByStatus groups put.io transfers by their status string
// (e.g. "COMPLETED", "SEEDING", "ERROR").
type transfersByStatus map[string][]*putio.Transfer

// TransferProcessor handles the processing of Put.io transfers
type TransferProcessor struct {
	manager   *Manager
	folderID  int64
	targetDir string

	// mu guards latest, which is published by the monitor goroutine on every
	// tick and read by RPC handler goroutines via GetTransfers.
	mu     sync.RWMutex
	latest []*putio.Transfer

	retryAttempts     sync.Map // map[int64]int - retry attempts for errored put.io transfers
	reprocessAttempts sync.Map // map[int64]int - local reprocess attempts for failed transfers
	stallTracker      *stallTracker
}

// GetTransfers returns the transfers seen on the most recent poll of the
// managed folders. Safe to call from any goroutine.
//
// The returned slice is a fresh copy, but the *putio.Transfer values are
// shared. That is safe because each poll builds new values and never mutates
// them afterwards.
func (p *TransferProcessor) GetTransfers() []*putio.Transfer {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.latest) == 0 {
		return nil
	}
	out := make([]*putio.Transfer, len(p.latest))
	copy(out, p.latest)
	return out
}

// setTransfers publishes the transfers from the latest poll.
func (p *TransferProcessor) setTransfers(transfers []*putio.Transfer) {
	p.mu.Lock()
	p.latest = transfers
	p.mu.Unlock()
}

// newTransferProcessor creates a new transfer processor
func newTransferProcessor(m *Manager) *TransferProcessor {
	return &TransferProcessor{
		manager:      m,
		folderID:     m.cfg.FolderID,
		targetDir:    m.cfg.TargetDir,
		stallTracker: newStallTracker(m.cfg.StalledTransferTimeout, time.Now),
	}
}

// monitorTransfers periodically checks for completed transfers
func (m *Manager) monitorTransfers() {
	log.Debug("transfers").Msg("Starting transfer monitor")

	log.Debug("transfers").
		Int64("folder_id", m.processor.folderID).
		Str("target_dir", m.processor.targetDir).
		Msg("Transfer processor initialized")

	// Initial check
	m.processor.checkTransfers()

	ticker := time.NewTicker(m.dlConfig.TransferCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopChan:
			log.Debug("transfers").Msg("Transfer monitor stopping")
			return
		case <-ticker.C:
			m.processor.checkTransfers()
		}
	}
}

// checkTransfers looks for completed or seeding transfers and processes them
func (p *TransferProcessor) checkTransfers() {
	log.Debug("transfers").Msg("Checking transfers")

	pending := p.manager.pendingRemovals()
	transfers, err := p.manager.client.GetTransfers(p.manager.Context())
	if err != nil {
		log.Error("transfers").Err(err).Msg("Failed to get transfers")
		return
	}
	p.manager.pruneRemovals(pending, transfers)

	log.Debug("transfers").
		Int("api_transfers_count", len(transfers)).
		Msg("Retrieved transfers from API")

	// Determine which put.io folders we consider "ours". This is the configured
	// folder plus, when put.io categorization is enabled, its category
	// subfolders.
	managed := p.managedFolders()

	// Keep only transfers in a managed folder, then group by status.
	inFolder := make([]*putio.Transfer, 0, len(transfers))
	byStatus := make(transfersByStatus)
	for _, t := range transfers {
		if _, ok := managed[t.SaveParentID]; !ok {
			log.Debug("transfers").
				Int64("transfer_id", t.ID).
				Int64("parent_id", t.SaveParentID).
				Int64("target_folder", p.folderID).
				Msg("Skipping transfer from different folder")
			continue
		}
		inFolder = append(inFolder, t)
		if p.manager.RemovalPending(t.ID) {
			continue
		}
		// Durable review holds take precedence over remote status, including
		// ERROR, so a restart cannot retry/delete an unverified record.
		if p.manager.restoreReview(t) {
			continue
		}
		byStatus[t.Status] = append(byStatus[t.Status], t)
	}

	p.stallTracker.Observe(inFolder)
	for _, transfer := range inFolder {
		if transfer.ErrorMessage == "" {
			transfer.ErrorMessage = p.stallTracker.Error(transfer.ID)
		}
	}

	// Publish for RPC handlers before doing any slow work.
	p.setTransfers(inFolder)

	p.logTransferSummary(byStatus, inFolder)

	// Process transfers by status
	p.processReadyTransfers(byStatus)
	p.processErroredTransfers(byStatus)

	// Check for transfers that are in "Completed" state but haven't been fully cleaned up
	p.finalizeCompletedTransfers()
}

// managedFolders returns the set of put.io folder IDs whose transfers this
// processor should handle. It always includes the configured folder. When
// put.io categorization is enabled, it also includes the configured folder's
// direct subfolders (the category folders such as "tv" or "movies").
//
// Only one level of category subfolders is discovered; deeply-nested put.io
// categories are not supported by the monitor.
func (p *TransferProcessor) managedFolders() map[int64]struct{} {
	managed := map[int64]struct{}{p.folderID: {}}

	if !p.manager.cfg.UseCategoriesPutio {
		return managed
	}

	files, err := p.manager.client.GetFiles(p.manager.Context(), p.folderID)
	if err != nil {
		log.Warn("transfers").
			Int64("folder_id", p.folderID).
			Err(err).
			Msg("Failed to list category subfolders; monitoring configured folder only")
		return managed
	}

	for _, f := range files {
		if f.IsDir() {
			managed[f.ID] = struct{}{}
		}
	}
	return managed
}

// logTransferSummary logs counts of transfers in each status and detailed information for all transfers
func (p *TransferProcessor) logTransferSummary(byStatus transfersByStatus, all []*putio.Transfer) {
	log.Info("transfers").
		Int("queued", len(byStatus["IN_QUEUE"])).
		Int("waiting", len(byStatus["WAITING"])).
		Int("preparing", len(byStatus["PREPARING"])).
		Int("downloading", len(byStatus["DOWNLOADING"])).
		Int("completing", len(byStatus["COMPLETING"])).
		Int("seeding", len(byStatus["SEEDING"])).
		Int("completed", len(byStatus["COMPLETED"])).
		Int("error", len(byStatus["ERROR"])).
		Msg("Transfer status summary")

	// Log detailed information for all transfers
	p.logAllTransfersDetails(all)
}

// logAllTransfersDetails logs detailed information for all transfers
func (p *TransferProcessor) logAllTransfersDetails(allTransfers []*putio.Transfer) {
	if len(allTransfers) == 0 {
		log.Debug("transfers").Msg("No transfers found for detailed logging")
		return
	}

	for _, t := range allTransfers {
		// Create a logger with common fields for all transfers
		transferLogger := log.Debug("transfers").
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

		// Whether we already downloaded and cleaned up this transfer locally.
		// The coordinator is the single source of truth for that.
		processed := false
		if ctx, ok := p.manager.coordinator.GetTransferContext(t.ID); ok {
			processed = ctx.GetState() == TransferLifecycleProcessed
		}
		transferLogger = transferLogger.Bool("processed_locally", processed)

		// Log the transfer details with a message that includes the status
		transferLogger.Msgf("Transfer details (%s)", t.Status)
	}
}

// processReadyTransfers handles completed and seeding transfers
func (p *TransferProcessor) processReadyTransfers(byStatus transfersByStatus) {
	completed := byStatus["COMPLETED"]
	readyTransfers := make([]*putio.Transfer, 0, len(completed)+len(byStatus["SEEDING"]))
	readyTransfers = append(readyTransfers, completed...)
	readyTransfers = append(readyTransfers, byStatus["SEEDING"]...)
	now := time.Now()

	for _, transfer := range readyTransfers {
		select {
		case <-p.manager.stopChan:
			log.Debug("transfers").Msg("Stopping transfer processing")
			return
		default:
			if !p.shouldProcess(transfer) {
				continue
			}
			if !CanStartDownloadNow(p.manager.cfg.DownloadStartWindow, now) {
				log.Debug("transfers").
					Int64("transfer_id", transfer.ID).
					Str("transfer_name", transfer.Name).
					Str("window_start", p.manager.cfg.DownloadStartWindow.Start).
					Str("window_end", p.manager.cfg.DownloadStartWindow.End).
					Msg("Skipping local download start because current time is outside the configured window")
				continue
			}
			p.startTransferProcessing(transfer)
		}
	}
}

// maxReprocessAttempts bounds how often a transfer whose local download
// failed is re-attempted, mirroring maxRetryAttempts for errored put.io
// transfers. Without a bound, a permanently broken file (a 404, a full disk)
// would be retried on every poll forever.
const maxReprocessAttempts = 3

// shouldProcess reports whether a ready transfer needs processing.
//
// An untracked transfer is new. A tracked one is normally left alone — it is
// either in flight or already done. The exception is a failed transfer: its
// context is dropped so the next poll sees it as new, up to
// maxReprocessAttempts times. Previously a failed transfer stayed tracked
// forever, so it was never retried, its put.io file was never cleaned up, and
// *arr never saw it complete.
func (p *TransferProcessor) shouldProcess(transfer *putio.Transfer) bool {
	p.manager.removalMu.RLock()
	defer p.manager.removalMu.RUnlock()
	if p.manager.RemovalPending(transfer.ID) || p.manager.NeedsReview(transfer.ID) {
		return false
	}
	ctx, exists := p.manager.coordinator.GetTransferContext(transfer.ID)
	if !exists {
		return true
	}

	if ctx.GetState() != TransferLifecycleFailed {
		log.Debug("transfers").
			Int64("transfer_id", transfer.ID).
			Str("state", ctx.GetState().String()).
			Msg("Transfer already being processed")
		return false
	}

	// Wait for the transfer to settle: retrying while siblings are still
	// downloading would queue a second generation of the same files.
	if active := p.manager.activeFileCount(transfer.ID); active > 0 {
		log.Debug("transfers").
			Int64("transfer_id", transfer.ID).
			Int("active_files", active).
			Msg("Failed transfer still has downloads in flight, deferring retry")
		return false
	}

	attempts := 0
	if v, ok := p.reprocessAttempts.Load(transfer.ID); ok {
		attempts = v.(int)
	}
	if attempts >= maxReprocessAttempts {
		log.Debug("transfers").
			Int64("transfer_id", transfer.ID).
			Str("name", transfer.Name).
			Int("attempts", attempts).
			Msg("Failed transfer exhausted reprocess attempts, leaving it alone")
		return false
	}

	p.reprocessAttempts.Store(transfer.ID, attempts+1)
	p.manager.coordinator.RemoveTransfer(transfer.ID)

	log.Info("transfers").
		Int64("transfer_id", transfer.ID).
		Str("name", transfer.Name).
		Int("attempt", attempts+1).
		Int("max_attempts", maxReprocessAttempts).
		Msg("Retrying transfer whose local download failed")
	return true
}

// forget drops all local bookkeeping for a transfer.
func (p *TransferProcessor) forget(transferID int64) {
	p.manager.coordinator.RemoveTransfer(transferID)
	p.retryAttempts.Delete(transferID)
	p.reprocessAttempts.Delete(transferID)
}

// startTransferProcessing begins processing a transfer
func (p *TransferProcessor) startTransferProcessing(transfer *putio.Transfer) {
	log.Info("transfers").
		Str("name", transfer.Name).
		Str("status", transfer.Status).
		Int64("id", transfer.ID).
		Msg("Found ready transfer")

	p.manager.processorWg.Add(1)
	transferCopy := *transfer
	go func() {
		defer p.manager.processorWg.Done()
		p.processTransfer(&transferCopy)
	}()
}

// processTransfer handles downloading of a completed or seeding transfer
func (p *TransferProcessor) processTransfer(transfer *putio.Transfer) {
	files, transferCtx := p.prepareTransfer(transfer)
	if transferCtx == nil {
		return
	}
	// Never hold removalMu across a channel send; a healthy file can occupy
	// a worker for hours. Each job instead checks its generation at admission.
	filesToDownload := p.queueTransferFiles(transfer, files, transferCtx)
	if filesToDownload == 0 {
		log.Info("transfers").Str("name", transfer.Name).Int64("transfer_id", transfer.ID).
			Msg("All files already exist, completing transfer")
		if err := p.manager.coordinator.CompleteTransfer(transfer.ID); err != nil {
			log.Error("transfers").Int64("transfer_id", transfer.ID).Err(err).
				Msg("Failed to complete transfer whose files already exist")
		}
	}
}

func (p *TransferProcessor) prepareTransfer(transfer *putio.Transfer) ([]*putio.File, *TransferContext) {
	// Reserve a generation before network I/O. Removal can forget it even if
	// its durable marker is reclaimed before the listing returns.
	p.manager.removalMu.RLock()
	if p.manager.RemovalPending(transfer.ID) || p.manager.NeedsReview(transfer.ID) {
		p.manager.removalMu.RUnlock()
		return nil, nil
	}
	reservation := p.manager.coordinator.InitiateTransfer(transfer.ID, transfer.Name, transfer.FileID, 0)
	p.manager.removalMu.RUnlock()

	var files []*putio.File
	var listErr error
	if transfer.FileID != 0 {
		files, listErr = p.manager.client.GetAllTransferFiles(p.manager.Context(), transfer.FileID)
	}
	p.manager.removalMu.RLock()
	defer p.manager.removalMu.RUnlock()
	current, exists := p.manager.coordinator.GetTransferContext(transfer.ID)
	if !exists || current != reservation || p.manager.RemovalPending(transfer.ID) {
		return nil, nil
	}
	log.Debug("transfers").
		Str("name", transfer.Name).
		Int64("id", transfer.ID).
		Int64("file_id", transfer.FileID).
		Msg("Processing transfer")

	// A zero file ID means Put.io has already removed the source. It also
	// identifies Put.io's root folder, so it must never be queried as though it
	// were a transfer file. This commonly occurs when Plundrio restarts after
	// local completion but before the Transmission client removes the transfer.
	if transfer.FileID == 0 {
		p.restoreCleanedTransfer(transfer)
		return nil, nil
	}

	if listErr != nil {
		// Preserve next-poll retries for ordinary transient listing errors.
		p.manager.coordinator.RemoveTransfer(transfer.ID)
		p.handleTransferError(transfer, listErr)
		return nil, nil
	}

	if len(files) == 0 {
		// Initialise before failing: FailTransfer needs a tracked context, so
		// doing this the other way round silently did nothing and left the
		// transfer to be re-examined on every poll forever.
		if !p.initializeTransfer(transfer, 0) {
			return nil, nil
		}
		if err := p.manager.coordinator.FailTransfer(transfer.ID, NewNoFilesFoundError(transfer.ID)); err != nil {
			log.Error("transfers").
				Int64("transfer_id", transfer.ID).
				Err(err).
				Msg("Failed to mark transfer as failed")
		}
		return nil, nil
	}

	// Track before building or persisting the manifest so any validation or
	// storage failure becomes an observable, bounded Failed transfer instead of
	// being retried as unseen work on every poll.
	if !p.initializeTransfer(transfer, len(files)) {
		return nil, nil
	}

	manifest, err := buildTransferFileManifest(transfer, files)
	if err != nil {
		log.Error("transfers").
			Int64("transfer_id", transfer.ID).
			Err(err).
			Msg("Failed to build transfer file manifest")
		p.failInitializedTransfer(transfer.ID, err)
		return nil, nil
	}
	if err := p.manager.transferFiles.Set(transfer.ID, manifest); err != nil {
		log.Error("transfers").
			Int64("transfer_id", transfer.ID).
			Err(err).
			Msg("Failed to persist transfer file manifest")
		p.failInitializedTransfer(transfer.ID, err)
		return nil, nil
	}

	// initializeTransfer replaces the reservation, preserving the immutable
	// file count expected by concurrent RPC readers.
	initialized, _ := p.manager.coordinator.GetTransferContext(transfer.ID)
	return files, initialized
}

func (p *TransferProcessor) failInitializedTransfer(transferID int64, err error) {
	if failErr := p.manager.coordinator.FailTransfer(transferID, err); failErr != nil {
		log.Error("transfers").
			Int64("transfer_id", transferID).
			Err(failErr).
			Msg("Failed to mark transfer as failed")
	}
}

func buildTransferFileManifest(transfer *putio.Transfer, files []*putio.File) ([]TransferFile, error) {
	transferName := filepath.Clean(transfer.Name)
	if !validRelativeTransferPath(transferName) || IsReservedTransferName(transferName) {
		return nil, fmt.Errorf("unsafe transfer name %q", transfer.Name)
	}

	manifest := make([]TransferFile, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		if file == nil {
			return nil, errors.New("transfer contains nil file metadata")
		}
		fileName := filepath.Clean(file.Name)
		if !validRelativeTransferPath(fileName) {
			return nil, fmt.Errorf("unsafe transfer file name %q", file.Name)
		}
		if file.Size < 0 {
			return nil, fmt.Errorf("transfer file %q has negative size", file.Name)
		}
		name := filepath.Join(transferName, fileName)
		name = filepath.ToSlash(name)
		if _, ok := seen[name]; ok {
			return nil, fmt.Errorf("multiple transfer files map to local path %q", name)
		}
		seen[name] = struct{}{}
		manifest = append(manifest, TransferFile{Name: name, Length: file.Size})
	}
	return manifest, nil
}

func (p *TransferProcessor) restoreCleanedTransfer(transfer *putio.Transfer) {
	files, err := p.manager.transferFiles.load(transfer.ID)
	if err != nil {
		p.failCleanedTransfer(transfer, err)
		return
	}
	if len(files) == 0 {
		// Source absence proves neither local completion nor file ownership.
		if err := p.manager.markNeedsReview(transfer); err != nil {
			log.Error("transfers").Int64("transfer_id", transfer.ID).Err(err).
				Msg("Could not persist review hold; repair state storage before restarting")
		}
		return
	}

	if err := p.validateLocalTransferFiles(transfer, files); err != nil {
		p.failCleanedTransfer(transfer, err)
		return
	}
	// The root is already absent; restoration must not issue a fresh delete.
	cleanedTransfer := *transfer
	cleanedTransfer.FileID = 0
	if !p.initializeTransfer(&cleanedTransfer, 0) {
		return
	}
	p.manager.cleanupTransfer(transfer.ID)
}

func validRelativeTransferPath(path string) bool {
	return path != "." && filepath.IsLocal(path)
}

func (p *TransferProcessor) validateLocalTransferFiles(transfer *putio.Transfer, files []TransferFile) error {
	downloadDir := filepath.Join(p.targetDir, p.manager.localCategory(transfer.ID))
	transferName := filepath.Clean(transfer.Name)
	if !validRelativeTransferPath(transferName) || IsReservedTransferName(transferName) {
		return fmt.Errorf("unsafe transfer name %q", transfer.Name)
	}

	for _, file := range files {
		name := filepath.Clean(filepath.FromSlash(file.Name))
		rel, err := filepath.Rel(transferName, name)
		if err != nil || !validRelativeTransferPath(rel) {
			return fmt.Errorf("manifest file %q escapes transfer directory", file.Name)
		}
		path := filepath.Join(downloadDir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("stat manifest file %q: %w", file.Name, err)
		}
		if !info.Mode().IsRegular() || info.Size() != file.Length {
			return fmt.Errorf("manifest file %q does not match expected length %d", file.Name, file.Length)
		}
	}
	return nil
}

func (p *TransferProcessor) failCleanedTransfer(transfer *putio.Transfer, err error) {
	if !p.initializeTransfer(transfer, 1) {
		return
	}
	if failErr := p.manager.coordinator.FailTransfer(transfer.ID, err); failErr != nil {
		log.Error("transfers").
			Int64("transfer_id", transfer.ID).
			Err(failErr).
			Msg("Failed to mark cleaned transfer as failed")
	}
}

// handleTransferError processes transfer errors appropriately
func (p *TransferProcessor) handleTransferError(transfer *putio.Transfer, err error) {
	var missing *api.TransferSourceNotFoundError
	if errors.As(err, &missing) && missing.FileID == transfer.FileID && isPutioNotFound(err) {
		log.Debug("transfers").
			Str("name", transfer.Name).
			Int64("id", transfer.ID).
			Msg("Source no longer exists on Put.io; checking local completion evidence")

		p.restoreCleanedTransfer(transfer)
		return
	}
	if isPutioNotFound(err) {
		// A child can disappear while the root remains. Keep this a bounded
		// listing failure, not evidence of legacy source absence.
		p.failCleanedTransfer(transfer, err)
		return
	}

	log.Error("transfers").
		Str("name", transfer.Name).
		Int64("id", transfer.ID).
		Err(err).
		Msg("Failed to get transfer files")
}

func isPutioNotFound(err error) bool {
	var putioErr *putio.ErrorResponse
	return errors.As(err, &putioErr) && putioErr.Type == "NotFound"
}

// queueTransferFiles processes files in a transfer and queues them for download
func (p *TransferProcessor) queueTransferFiles(transfer *putio.Transfer, files []*putio.File, ctx *TransferContext) int {
	filesToDownload := 0

	// Calculate total size of all files
	var totalSize int64
	for _, file := range files {
		totalSize += file.Size
	}

	// Update the transfer context with total size
	ctx.SetTotalSize(totalSize)

	log.Info("transfers").
		Int64("transfer_id", transfer.ID).
		Int64("total_size", totalSize).
		Int("file_count", len(files)).
		Msg("Updated transfer with total file size")

	for _, file := range files {
		if p.shouldDownloadFile(transfer, file) {
			filesToDownload++
			p.queueFileDownload(transfer, file, ctx)
		} else {
			// For files we don't need to download (already exist), mark as completed
			if err := p.manager.coordinator.FileCompleted(transfer.ID); err != nil {
				log.Error("transfers").
					Int64("transfer_id", transfer.ID).
					Str("file_name", file.Name).
					Err(err).
					Msg("Failed to mark existing file as completed")
			}

			// For existing files, add their size to the downloaded size
			ctx.AddDownloadedBytes(file.Size)

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
	category := p.manager.localCategory(transfer.ID)
	targetPath := filepath.Join(p.targetDir, category, transfer.Name, file.Name)
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
func (p *TransferProcessor) queueFileDownload(transfer *putio.Transfer, file *putio.File, ctx *TransferContext) {
	category := p.manager.localCategory(transfer.ID)
	p.manager.queueDownload(downloadJob{
		FileID:     file.ID,
		Name:       filepath.Join(category, transfer.Name, file.Name),
		TransferID: transfer.ID,
	}, ctx)
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
		if failErr := p.manager.coordinator.FailTransfer(transfer.ID, err); failErr != nil {
			log.Error("transfers").
				Int64("id", transfer.ID).
				Err(failErr).
				Msg("Failed to mark transfer as failed")
		}
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
func (p *TransferProcessor) processErroredTransfers(byStatus transfersByStatus) {
	const maxRetryAttempts = 3

	for _, transfer := range byStatus["ERROR"] {
		if p.manager.RemovalPending(transfer.ID) || p.manager.NeedsReview(transfer.ID) {
			continue
		}
		if ctx, ok := p.manager.GetTransferContext(transfer.ID); ok && ctx.GetState() == TransferLifecycleNeedsReview {
			continue
		}
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
			retried, err := p.manager.client.RetryTransfer(p.manager.Context(), transfer.ID)
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
			}
		} else {
			// Log that we're giving up after max retries
			logger.Msgf("Transfer errored, giving up after %d retry attempts", maxRetryAttempts)

			// Delete the transfer after max retries
			if err := p.manager.client.DeleteTransfer(p.manager.Context(), transfer.ID); err != nil {
				log.Error("transfers").
					Str("name", transfer.Name).
					Int64("id", transfer.ID).
					Err(err).
					Msg("Failed to delete errored transfer after max retries")
			} else {
				// Clear retry counter after successful deletion
				p.retryAttempts.Delete(transfer.ID)
				log.Info("transfers").
					Str("name", transfer.Name).
					Int64("id", transfer.ID).
					Msg("Deleted errored transfer after max retries")
			}
		}
	}
}

// finalizeCompletedTransfers checks for transfers that are marked as completed in the
// internal tracking system but haven't been fully cleaned up yet.
func (p *TransferProcessor) finalizeCompletedTransfers() {
	// Get all active transfers from the coordinator
	var pendingCleanup []int64

	p.manager.coordinator.RangeTransfers(func(transferID int64, ctx *TransferContext) bool {

		state := ctx.GetState()

		if state == TransferLifecycleCompleted {
			log.Info("transfers").
				Int64("id", transferID).
				Str("name", ctx.Name).
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

		if err := p.manager.coordinator.CompleteTransfer(transferID); err != nil {
			log.Error("transfers").
				Int64("id", transferID).
				Err(err).
				Msg("Failed to finalize completed transfer")
		}
	}
}
