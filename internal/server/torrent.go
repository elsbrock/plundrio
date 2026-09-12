package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/download"
	"github.com/elsbrock/plundrio/internal/log"
)

const (
	transmissionLimitModeSingle    = 1
	transmissionLimitModeUnlimited = 2
)

// extractCategory returns the relative category path from downloadDir.
// For example, if targetDir="/downloads" and downloadDir="/downloads/tv",
// it returns "tv". Returns "" if downloadDir is empty, equals targetDir, or
// resolves to a location outside targetDir (path traversal), since the category
// is used to build local filesystem paths.
func extractCategory(targetDir, downloadDir string) string {
	if downloadDir == "" {
		return ""
	}
	rel, err := filepath.Rel(targetDir, downloadDir)
	if err != nil || rel == "." {
		return ""
	}
	rel = filepath.Clean(rel)
	// Reject categories that escape targetDir (e.g. downloadDir="/etc").
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return ""
	}
	return rel
}

// localCategory returns the category subfolder for a transfer's local path, or
// "" when local categorization is disabled.
func (s *Server) localCategory(transferID int64) string {
	if !s.cfg.UseCategoriesTarget {
		return ""
	}
	return s.dlService.GetCategory(transferID)
}

// torrentID is a Transmission torrent selector. Transmission clients may use
// either the numeric torrent ID or its hash string.
type torrentID struct {
	id      int64
	hash    string
	numeric bool
}

func (id *torrentID) UnmarshalJSON(data []byte) error {
	value := strings.TrimSpace(string(data))
	if value == "" {
		return fmt.Errorf("empty torrent id")
	}

	if strings.HasPrefix(value, `"`) {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		if text == "" {
			return fmt.Errorf("empty torrent id")
		}
		id.hash = text
		return nil
	}

	numericID, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fmt.Errorf("torrent id must be an integer or hash string: %w", err)
	}
	id.id = numericID
	id.numeric = true
	return nil
}

func (id torrentID) matches(transfer *putio.Transfer) bool {
	if id.numeric {
		return transfer.ID == id.id
	}
	return strings.EqualFold(transfer.Hash, id.hash)
}

func (id torrentID) String() string {
	if id.numeric {
		return strconv.FormatInt(id.id, 10)
	}
	return id.hash
}

type torrentIDs []torrentID

func (ids torrentIDs) matches(transfer *putio.Transfer) bool {
	return len(ids) == 0 || slices.ContainsFunc(ids, func(id torrentID) bool {
		return id.matches(transfer)
	})
}

// findTransfer resolves either a numeric Transmission ID or a torrent hash.
func (s *Server) findTransfer(ctx context.Context, id torrentID) (*putio.Transfer, error) {
	transfers, err := s.client.GetTransfers(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range transfers {
		if id.matches(t) {
			return t, nil
		}
	}
	return nil, fmt.Errorf("transfer not found with id: %s", id.String())
}

// handleTorrentAdd processes torrent-add requests
func (s *Server) handleTorrentAdd(ctx context.Context, args json.RawMessage) (interface{}, error) {
	var params struct {
		Filename    string `json:"filename"`     // For .torrent files
		MetaInfo    string `json:"metainfo"`     // Base64 encoded .torrent
		MagnetLink  string `json:"magnetLink"`   // Magnet link
		DownloadDir string `json:"download-dir"` // Category dir from the *arr app (e.g. /downloads/tv)
	}

	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	category := extractCategory(s.cfg.TargetDir, params.DownloadDir)

	// Resolve the put.io folder to add the transfer to. With put.io
	// categorization enabled, transfers for a category land in a subfolder.
	folderID, err := s.putioFolderForCategory(ctx, category)
	if err != nil {
		return nil, err
	}

	var name string
	var transfer *putio.Transfer

	// Handle .torrent file upload if metainfo is provided
	if params.MetaInfo != "" {
		// Decode base64 torrent data
		torrentData, err := base64.StdEncoding.DecodeString(params.MetaInfo)
		if err != nil {
			return nil, fmt.Errorf("failed to decode torrent data: %w", err)
		}

		// Upload torrent file to Put.io
		name = params.Filename
		if name == "" {
			name = "unknown.torrent"
		}
		uploadedTransfer, err := s.client.UploadFile(ctx, torrentData, name, folderID)
		if err != nil {
			return nil, fmt.Errorf("failed to upload torrent: %w", err)
		}
		transfer = uploadedTransfer

		log.Info("rpc").
			Str("operation", "torrent-add").
			Str("type", "torrent").
			Str("name", name).
			Str("category", category).
			Int64("folder_id", folderID).
			Msg("Torrent file uploaded")
	} else {
		// Handle magnet links
		if params.MagnetLink != "" {
			name = params.MagnetLink
		} else if params.Filename != "" && strings.HasPrefix(params.Filename, "magnet:") {
			name = params.Filename
		} else {
			return nil, fmt.Errorf("invalid torrent or magnet link provided")
		}

		// Add magnet link to Put.io
		addedTransfer, err := s.client.AddTransfer(ctx, name, folderID)
		if err != nil {
			return nil, fmt.Errorf("failed to add transfer: %w", err)
		}
		transfer = addedTransfer

		log.Info("rpc").
			Str("operation", "torrent-add").
			Str("type", "magnet").
			Str("magnet", name).
			Str("category", category).
			Int64("folder_id", folderID).
			Msg("Magnet link added")
	}

	if err := validateAddedTransfer(transfer); err != nil {
		return nil, err
	}
	if transfer.Hash == "" {
		log.Warn("rpc").
			Str("operation", "torrent-add").
			Int64("id", transfer.ID).
			Str("name", transfer.Name).
			Msg("Put.io returned transfer without info hash")
	}

	// Store the category for the transfer so that local downloads land in the
	// right subfolder. Keyed by transfer ID, which put.io always populates,
	// unlike the info hash. Only needed when local categorization is enabled.
	if category != "" && s.cfg.UseCategoriesTarget {
		s.dlService.SetCategory(transfer.ID, category)
		log.Info("rpc").
			Str("operation", "torrent-add").
			Int64("transfer_id", transfer.ID).
			Str("category", category).
			Msg("Stored category for transfer")
	}

	return torrentAddedResponse(transfer, name), nil
}

func validateAddedTransfer(transfer *putio.Transfer) error {
	if transfer == nil {
		return fmt.Errorf("put.io did not return the created transfer")
	}
	if transfer.ID == 0 {
		return fmt.Errorf("put.io returned incomplete transfer metadata")
	}
	return nil
}

func torrentAddedResponse(transfer *putio.Transfer, fallbackName string) map[string]interface{} {
	name := transfer.Name
	if name == "" {
		name = fallbackName
	}

	return map[string]interface{}{
		"torrent-added": map[string]interface{}{
			"id":         transfer.ID,
			"hashString": transfer.Hash,
			"name":       name,
		},
	}
}

// putioFolderForCategory returns the put.io folder ID that a transfer with the
// given category should be added to. When put.io categorization is disabled or
// the category is empty, the configured folder is returned. Otherwise the
// category subfolder is created (if needed) under the configured folder.
func (s *Server) putioFolderForCategory(ctx context.Context, category string) (int64, error) {
	if !s.cfg.UseCategoriesPutio || category == "" {
		return s.cfg.FolderID, nil
	}

	s.catFolderMu.Lock()
	defer s.catFolderMu.Unlock()

	if id, ok := s.catFolders[category]; ok {
		return id, nil
	}

	// Walk the (possibly nested) category path, ensuring each segment exists.
	parent := s.cfg.FolderID
	for _, segment := range strings.Split(filepath.ToSlash(category), "/") {
		if segment == "" {
			continue
		}
		id, err := s.client.EnsureFolderInParent(ctx, segment, parent)
		if err != nil {
			return 0, fmt.Errorf("failed to ensure put.io category folder %q: %w", category, err)
		}
		parent = id
	}

	s.catFolders[category] = parent
	log.Info("rpc").
		Str("operation", "torrent-add").
		Str("category", category).
		Int64("folder_id", parent).
		Msg("Resolved put.io category folder")
	return parent, nil
}

// handleTorrentGet processes torrent-get requests
func (s *Server) handleTorrentGet(_ context.Context, args json.RawMessage) (interface{}, error) {
	var params struct {
		IDs    torrentIDs `json:"ids"`
		Fields []string   `json:"fields"`
	}

	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	// Log input parameters
	log.Debug("rpc").
		Str("operation", "torrent-get").
		Interface("ids", params.IDs).
		Interface("fields", params.Fields).
		Msg("Processing torrent-get request")

	transfers := s.dlService.GetTransfers()
	if transfers == nil {
		return map[string]interface{}{
			"torrents": []map[string]interface{}{},
		}, nil
	}

	log.Debug("rpc").
		Str("operation", "torrent-get").
		Int("all_transfers_count", len(transfers)).
		Msg("Retrieved all transfers from processor")

	// Convert Put.io transfers to transmission format
	torrents := make([]map[string]interface{}, 0, len(transfers))
	for _, t := range transfers {
		// Filter by IDs if specified
		if !params.IDs.matches(t) {
			continue
		}

		// Look up transfer context if available
		var transferCtx *download.TransferContext
		if ctx, exists := s.dlService.GetTransferContext(t.ID); exists {
			transferCtx = ctx
		}

		// Calculate combined progress
		prog := calculateProgress(progressInput{
			PutioPercentDone: t.PercentDone,
			PutioStatus:      t.Status,
			PutioSize:        t.Size,
			TransferCtx:      transferCtx,
		})

		percentDone := prog.PercentDone
		status := prog.Status
		leftUntilDone := prog.LeftUntilDone
		eta := t.EstimatedTime
		rateDownload := t.DownloadSpeed
		seedIdleMode := transmissionLimitModeUnlimited
		secondsSeeding := int64(0)

		// Arr clients treat a seeding torrent as removable only after its
		// per-torrent idle limit has been exceeded. Put.io owns seeding, so a
		// processed local transfer can report that limit as reached. Remote
		// completion alone must never authorize removal before the local copy.
		if transferCtx != nil && transferCtx.GetState() == download.TransferLifecycleProcessed &&
			status == trStatusSeed && leftUntilDone == 0 {
			seedIdleMode = transmissionLimitModeSingle
			secondsSeeding = 1
		}

		// Override ETA and rate with local values when available
		if !prog.LocalETA.IsZero() {
			if secsUntil := int64(time.Until(prog.LocalETA).Seconds()); secsUntil > 0 {
				eta = secsUntil
			}
			if prog.LocalSpeed > 0 {
				rateDownload = int(prog.LocalSpeed)
			}
		}

		log.Debug("rpc").
			Str("operation", "torrent-get").
			Int64("id", t.ID).
			Str("name", t.Name).
			Float64("percent_done", percentDone*100).
			Int64("left_until_done", leftUntilDone).
			Int("status", status).
			Msg("Calculated progress")

		torrentInfo := map[string]interface{}{
			"id":             t.ID,
			"hashString":     t.Hash,
			"name":           t.Name,
			"eta":            eta,
			"status":         status,
			"downloadDir":    filepath.Join(s.cfg.TargetDir, s.localCategory(t.ID)),
			"totalSize":      t.Size,
			"leftUntilDone":  leftUntilDone,
			"uploadedEver":   t.Uploaded,
			"downloadedEver": t.Downloaded,
			"percentDone":    percentDone,
			"rateDownload":   rateDownload,
			"rateUpload":     t.UploadSpeed,
			"secondsSeeding": secondsSeeding,
			"seedRatioLimit": 0,
			"seedRatioMode":  transmissionLimitModeUnlimited,
			"seedIdleLimit":  0,
			"seedIdleMode":   seedIdleMode,
			"uploadRatio": func() float64 {
				if t.Size > 0 {
					return float64(t.Uploaded) / float64(t.Size)
				}
				return 0
			}(),
			"error":       t.ErrorMessage != "",
			"errorString": t.ErrorMessage,
		}

		if slices.Contains(params.Fields, "files") {
			files, err := s.localTorrentFiles(t, percentDone >= 1)
			if err != nil {
				return nil, fmt.Errorf("list local files for transfer %d: %w", t.ID, err)
			}
			torrentInfo["files"] = files
		}
		if s.dlService.RemovalPending(t.ID) {
			// A failed removal is terminal local state, even without a context.
			torrentInfo["status"] = trStatusStopped
			torrentInfo["rateDownload"] = 0
			torrentInfo["error"] = true
			torrentInfo["errorString"] = "remote deletion pending; retry torrent-remove or remove the transfer on Put.io"
		}
		if s.dlService.NeedsReview(t.ID) || (transferCtx != nil && transferCtx.GetState() == download.TransferLifecycleNeedsReview) {
			applyReviewStatus(torrentInfo, t.Size, slices.Contains(params.Fields, "files"))
			if s.dlService.RemovalPending(t.ID) {
				torrentInfo["errorString"] = "Reviewed record retirement pending; retry the explicit reviewed-retirement request. Local files remain unverified and untouched."
			}
		}

		torrents = append(torrents, torrentInfo)

		// Log each torrent being added to the response
		log.Debug("rpc").
			Str("operation", "torrent-get").
			Int64("id", t.ID).
			Str("hash", t.Hash).
			Str("name", t.Name).
			Str("status", t.Status).
			Int("size", t.Size).
			Float64("percent_done", percentDone).
			Msg("Added torrent to response")
	}

	// Log the final count of torrents in the response
	log.Debug("rpc").
		Str("operation", "torrent-get").
		Int("torrents_count", len(torrents)).
		Msg("Returning torrents")

	result := map[string]interface{}{
		"torrents": torrents,
	}

	// Log the final response structure
	resultBytes, _ := json.Marshal(result)
	log.Debug("rpc").
		Str("operation", "torrent-get").
		Str("result", string(resultBytes)).
		Msg("Final result structure")

	return result, nil
}

type transmissionFile struct {
	BytesCompleted int64  `json:"bytesCompleted"`
	Length         int64  `json:"length"`
	Name           string `json:"name"`
}

// localTorrentFiles returns the transfer-ID-keyed manifest. Names are relative
// to downloadDir, matching Transmission's files contract.
func (s *Server) localTorrentFiles(transfer *putio.Transfer, complete bool) ([]transmissionFile, error) {
	transferName := filepath.Clean(transfer.Name)
	if transferName == "." || !filepath.IsLocal(transferName) {
		return nil, fmt.Errorf("unsafe transfer path %q", transfer.Name)
	}
	manifest, ok := s.dlService.GetTransferFiles(transfer.ID)
	if !ok {
		// Put.io may be complete before the local processor has built its
		// authoritative manifest. Keep the torrent in the RPC response without
		// claiming ownership of any files yet.
		return []transmissionFile{}, nil
	}
	files := make([]transmissionFile, 0, len(manifest))
	for _, file := range manifest {
		if file.Length < 0 {
			return nil, fmt.Errorf("manifest file %q has negative length", file.Name)
		}
		name := filepath.Clean(filepath.FromSlash(file.Name))
		rel, err := filepath.Rel(transferName, name)
		if err != nil || rel == "." || !filepath.IsLocal(rel) {
			return nil, fmt.Errorf("manifest file %q is outside transfer %q", file.Name, transfer.Name)
		}
		var bytesCompleted int64
		// Whole-transfer granularity: per-file progress is not persisted.
		// Report each file's full length only after the local transfer completes.
		if complete {
			bytesCompleted = file.Length
		}
		files = append(files, transmissionFile{
			BytesCompleted: bytesCompleted,
			Length:         file.Length,
			Name:           filepath.ToSlash(name),
		})
	}
	return files, nil
}

// handleTorrentRemove processes torrent-remove requests
func (s *Server) handleTorrentRemove(ctx context.Context, args json.RawMessage) (interface{}, error) {
	var params struct {
		IDs             torrentIDs `json:"ids"`
		DeleteLocalData bool       `json:"delete-local-data"`
		RetireReviewed  bool       `json:"plundrio-retire-reviewed"`
		CopyVerified    bool       `json:"plundrio-copy-verified"`
	}

	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if params.RetireReviewed || params.CopyVerified {
		if !params.RetireReviewed || !params.CopyVerified || params.DeleteLocalData || len(params.IDs) != 1 || !params.IDs[0].numeric || params.IDs[0].id <= 0 {
			return nil, fmt.Errorf("review retirement requires one exact positive numeric ID, delete-local-data=false, plundrio-retire-reviewed=true and plundrio-copy-verified=true")
		}
		return s.retireReviewedTransfer(ctx, params.IDs[0])
	}

	// Keep bulk removal best-effort: one transfer's safety guard or remote
	// failure must not prevent later IDs from being attempted.
	var removalErrors []error
	for _, id := range params.IDs {
		transfer, err := s.findTransfer(ctx, id)
		if err != nil {
			log.Error("rpc").
				Str("operation", "torrent-remove").
				Str("id", id.String()).
				Err(err).
				Msg("Failed to find transfer")
			continue
		}

		// Capture the deletion destination before remote mutation: the monitor
		// may reclaim the durable category as soon as remote absence is visible.
		// Ready remote records must be classified before cancellation; the
		// manager checks this under the same lock as retry generation changes.
		ready := transfer.Status == "COMPLETED" || transfer.Status == "SEEDING"
		category, err := s.dlService.PrepareRemoval(transfer.ID, ready)
		if err != nil {
			removalErrors = append(removalErrors, fmt.Errorf("preserve removal state for transfer %d: %w", transfer.ID, err))
			continue
		}
		if !s.cfg.UseCategoriesTarget {
			category = ""
		}

		// Seeding-only transfers (where the file was already deleted) have no
		// file_id. Calling DeleteFile(0) would target the root folder and
		// cascade-delete everything in the account.
		if transfer.FileID == 0 {
			log.Warn("rpc").
				Str("operation", "torrent-remove").
				Str("id", id.String()).
				Int64("transfer_id", transfer.ID).
				Msg("Skipping file deletion: transfer has no associated file")
		} else if err := s.client.DeleteFile(ctx, transfer.FileID); err != nil {
			log.Error("rpc").
				Str("operation", "torrent-remove").
				Str("id", id.String()).
				Int64("transfer_id", transfer.ID).
				Err(err).
				Msg("Failed to delete transfer files")
		}

		const maxRemovalAttempts = 3
		var removalErr error
		for attempt := 0; attempt < maxRemovalAttempts; attempt++ {
			removalErr = s.client.DeleteTransfer(ctx, transfer.ID)
			if removalErr == nil || ctx.Err() != nil {
				break
			}
		}
		if removalErr != nil {
			log.Error("rpc").
				Str("operation", "torrent-remove").
				Str("id", id.String()).
				Int64("transfer_id", transfer.ID).
				Err(removalErr).
				Msg("Failed to delete transfer")
			removalErrors = append(removalErrors, fmt.Errorf("remove transfer %d after at most %d attempts; local processing suspended, retry torrent-remove: %w", transfer.ID, maxRemovalAttempts, removalErr))
			continue
		} else {
			log.Info("rpc").
				Str("operation", "torrent-remove").
				Str("id", id.String()).
				Int64("transfer_id", transfer.ID).
				Bool("delete_local_data", params.DeleteLocalData).
				Msg("Transfer removed")
		}

		if params.DeleteLocalData {
			localTargetDir := filepath.Join(s.cfg.TargetDir, category)
			if err := deleteLocalData(localTargetDir, transfer.Name); err != nil {
				log.Error("rpc").
					Str("operation", "torrent-remove").
					Str("transfer_name", transfer.Name).
					Str("category", category).
					Err(err).
					Msg("Failed to delete local files")
			} else {
				log.Info("rpc").
					Str("operation", "torrent-remove").
					Str("transfer_name", transfer.Name).
					Str("category", category).
					Msg("Deleted local files")
			}
		}

		s.dlService.RemoveCategory(transfer.ID)
		s.dlService.RemoveTransfer(transfer.ID)
	}

	if err := errors.Join(removalErrors...); err != nil {
		return nil, err
	}
	return struct{}{}, nil
}

// deleteLocalData removes downloaded files for a transfer from the target directory.
// It validates that the resolved path is inside targetDir to prevent path traversal.
func deleteLocalData(targetDir, transferName string) error {
	if download.IsReservedTransferName(transferName) {
		return fmt.Errorf("transfer name %q is reserved for Plundrio state", transferName)
	}
	localPath := filepath.Join(targetDir, transferName)
	absLocal, err := filepath.Abs(localPath)
	if err != nil {
		return fmt.Errorf("failed to resolve local path %q: %w", localPath, err)
	}
	absTarget, err := filepath.Abs(targetDir)
	if err != nil {
		return fmt.Errorf("failed to resolve target dir %q: %w", targetDir, err)
	}
	if !strings.HasPrefix(absLocal, absTarget+string(os.PathSeparator)) {
		return fmt.Errorf("path %q is outside target directory %q", absLocal, absTarget)
	}
	return os.RemoveAll(absLocal)
}
