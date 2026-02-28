package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/download"
	"github.com/elsbrock/plundrio/internal/log"
)

// extractCategory returns the relative category path from downloadDir.
// For example, if targetDir="/downloads" and downloadDir="/downloads/tv",
// it returns "tv". Returns "" if downloadDir is empty or equals targetDir.
func extractCategory(targetDir, downloadDir string) string {
	if downloadDir == "" {
		return ""
	}
	rel, err := filepath.Rel(targetDir, downloadDir)
	if err != nil || rel == "." {
		return ""
	}
	// Clean up any trailing slashes or path oddities
	return filepath.Clean(rel)
}

// findTransferByHash finds a transfer by its hash string
func (s *Server) findTransferByHash(ctx context.Context, hash string) (*putio.Transfer, error) {
	transfers, err := s.client.GetTransfers(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range transfers {
		if t.Hash == hash {
			return t, nil
		}
	}
	return nil, fmt.Errorf("transfer not found with hash: %s", hash)
}

// handleTorrentAdd processes torrent-add requests
func (s *Server) handleTorrentAdd(ctx context.Context, args json.RawMessage) (interface{}, error) {
	var params struct {
		Filename    string `json:"filename"`    // For .torrent files
		MetaInfo    string `json:"metainfo"`    // Base64 encoded .torrent
		MagnetLink  string `json:"magnetLink"`  // Magnet link
		DownloadDir string `json:"downloadDir"` // Category subfolder (e.g. /downloads/tv)
	}

	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	category := extractCategory(s.cfg.TargetDir, params.DownloadDir)
	var name string
	var hash string

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
		h, err := s.client.UploadFile(ctx, torrentData, name, s.cfg.FolderID)
		if err != nil {
			return nil, fmt.Errorf("failed to upload torrent: %w", err)
		}
		hash = h

		log.Info("rpc").
			Str("operation", "torrent-add").
			Str("type", "torrent").
			Str("name", name).
			Str("category", category).
			Int64("folder_id", s.cfg.FolderID).
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
		h, err := s.client.AddTransfer(ctx, name, s.cfg.FolderID)
		if err != nil {
			return nil, fmt.Errorf("failed to add transfer: %w", err)
		}
		hash = h

		log.Info("rpc").
			Str("operation", "torrent-add").
			Str("type", "magnet").
			Str("magnet", name).
			Str("category", category).
			Int64("folder_id", s.cfg.FolderID).
			Msg("Magnet link added")
	}

	// Store category mapping if we have both a hash and a category
	if hash != "" && category != "" {
		s.dlService.SetCategory(hash, category)
		log.Info("rpc").
			Str("operation", "torrent-add").
			Str("hash", hash).
			Str("category", category).
			Msg("Stored category for transfer")
	}

	// Return success response
	return map[string]interface{}{
		"torrent-added": map[string]interface{}{},
	}, nil
}

// handleTorrentGet processes torrent-get requests
func (s *Server) handleTorrentGet(_ context.Context, args json.RawMessage) (interface{}, error) {
	var params struct {
		IDs    []string `json:"ids"`
		Fields []string `json:"fields"`
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
		if len(params.IDs) > 0 {
			found := false
			for _, id := range params.IDs {
				if id == t.Hash {
					found = true
					break
				}
			}
			if !found {
				continue
			}
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
			"downloadDir":    filepath.Join(s.cfg.TargetDir, s.dlService.GetCategory(t.Hash)),
			"totalSize":      t.Size,
			"leftUntilDone":  leftUntilDone,
			"uploadedEver":   t.Uploaded,
			"downloadedEver": t.Downloaded,
			"percentDone":    percentDone,
			"rateDownload":   rateDownload,
			"rateUpload":     t.UploadSpeed,
			"uploadRatio": func() float64 {
				if t.Size > 0 {
					return float64(t.Uploaded) / float64(t.Size)
				}
				return 0
			}(),
			"error":       t.ErrorMessage != "",
			"errorString": t.ErrorMessage,
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

// handleTorrentRemove processes torrent-remove requests
func (s *Server) handleTorrentRemove(ctx context.Context, args json.RawMessage) (interface{}, error) {
	var params struct {
		IDs             []string `json:"ids"`
		DeleteLocalData bool     `json:"delete-local-data"`
	}

	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	for _, hash := range params.IDs {
		transfer, err := s.findTransferByHash(ctx, hash)
		if err != nil {
			log.Error("rpc").
				Str("operation", "torrent-remove").
				Str("hash", hash).
				Err(err).
				Msg("Failed to find transfer")
			continue
		}

		// Seeding-only transfers (where the file was already deleted) have no
		// file_id. Calling DeleteFile(0) would target the root folder and
		// cascade-delete everything in the account.
		if transfer.FileID == 0 {
			log.Warn("rpc").
				Str("operation", "torrent-remove").
				Str("hash", hash).
				Int64("transfer_id", transfer.ID).
				Msg("Skipping file deletion: transfer has no associated file")
		} else if err := s.client.DeleteFile(ctx, transfer.FileID); err != nil {
			log.Error("rpc").
				Str("operation", "torrent-remove").
				Str("hash", hash).
				Int64("transfer_id", transfer.ID).
				Err(err).
				Msg("Failed to delete transfer files")
		}

		if err := s.client.DeleteTransfer(ctx, transfer.ID); err != nil {
			log.Error("rpc").
				Str("operation", "torrent-remove").
				Str("hash", hash).
				Int64("transfer_id", transfer.ID).
				Err(err).
				Msg("Failed to delete transfer")
		} else {
			log.Info("rpc").
				Str("operation", "torrent-remove").
				Str("hash", hash).
				Int64("transfer_id", transfer.ID).
				Bool("delete_local_data", params.DeleteLocalData).
				Msg("Transfer removed")
		}

		// Delete local files if requested (closes #23)
		if params.DeleteLocalData {
			category := s.dlService.GetCategory(hash)
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

		// Clean up category mapping
		s.dlService.RemoveCategory(hash)
	}

	return struct{}{}, nil
}

// deleteLocalData removes downloaded files for a transfer from the target directory.
// It validates that the resolved path is inside targetDir to prevent path traversal.
func deleteLocalData(targetDir, transferName string) error {
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
