package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/log"
)

// findTransferByHash finds a transfer by its hash string
func (s *Server) findTransferByHash(hash string) (*putio.Transfer, error) {
	transfers, err := s.client.GetTransfers()
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
func (s *Server) handleTorrentAdd(args json.RawMessage) (interface{}, error) {
	var params struct {
		Filename    string `json:"filename"`    // For .torrent files
		MetaInfo    string `json:"metainfo"`    // Base64 encoded .torrent
		MagnetLink  string `json:"magnetLink"`  // Magnet link
		DownloadDir string `json:"downloadDir"` // Ignored, we use Put.io
	}

	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	var name string

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
		if err := s.client.UploadFile(torrentData, name, s.cfg.FolderID); err != nil {
			return nil, fmt.Errorf("failed to upload torrent: %w", err)
		}

		log.Info("rpc").
			Str("operation", "torrent-add").
			Str("type", "torrent").
			Str("name", name).
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
		if err := s.client.AddTransfer(name, s.cfg.FolderID); err != nil {
			return nil, fmt.Errorf("failed to add transfer: %w", err)
		}

		log.Info("rpc").
			Str("operation", "torrent-add").
			Str("type", "magnet").
			Str("magnet", name).
			Int64("folder_id", s.cfg.FolderID).
			Msg("Magnet link added")

		// Return success response
		return map[string]interface{}{
			"torrent-added": map[string]interface{}{},
		}, nil
	}

	// Return success response
	return map[string]interface{}{
		"torrent-added": map[string]interface{}{},
	}, nil
}

// handleTorrentGet processes torrent-get requests
func (s *Server) handleTorrentGet(args json.RawMessage) (interface{}, error) {
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

	// Get all transfers directly from put.io instead of just from the processor
	putioTransfers, err := s.client.GetTransfers()
	if err != nil {
		return nil, fmt.Errorf("failed to get transfers from put.io: %w", err)
	}

	// Filter transfers for our target folder
	var transfers []*putio.Transfer
	for _, t := range putioTransfers {
		if t.SaveParentID == s.cfg.FolderID {
			transfers = append(transfers, t)
		}
	}

	log.Debug("rpc").
		Str("operation", "torrent-get").
		Int("all_transfers_count", len(transfers)).
		Msg("Retrieved all transfers")

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

		// Calculate combined progress
		var percentDone float64
		var status int

		// Check if we have a transfer context (transfer is being processed)
		if ctx, exists := s.dlManager.GetCoordinator().GetTransferContext(t.ID); exists && ctx.TotalFiles > 0 {
			// For transfers being processed, use the 0-50% and 50-100% rule
			// Always map put.io progress to 0-50%
			putioProgress := float64(t.PercentDone) / 200.0 // Maps 0-100 to 0-0.5

			// Add local download progress (50-100%) if files are being downloaded
			localProgress := float64(ctx.CompletedFiles) / float64(ctx.TotalFiles)
			percentDone = putioProgress + (localProgress * 0.5) // Maps 0-1 to 0.5-1.0

			// Only set status to completed/seeding if all files are downloaded locally
			if ctx.State == 2 { // TransferLifecycleCompleted = 2
				status = s.mapPutioStatus(t.Status)
			} else {
				// If not all files are downloaded, show as downloading
				status = 4 // TR_STATUS_DOWNLOAD
			}
		} else if t.Status == "COMPLETED" || t.Status == "SEEDING" {
			// For transfers that are completed on put.io but have no corresponding entry in the processor
			// (i.e., already downloaded), show as 100% complete with status "downloaded"
			percentDone = 1.0 // 100%
			status = 6        // TR_STATUS_SEED (completed/seeding)
		} else {
			// For other transfers not being processed, just use put.io progress (0-50%)
			putioProgress := float64(t.PercentDone) / 200.0 // Maps 0-100 to 0-0.5
			percentDone = putioProgress
			status = s.mapPutioStatus(t.Status)
		}

		torrentInfo := map[string]interface{}{
			"id":             t.ID,
			"hashString":     t.Hash,
			"name":           t.Name,
			"status":         status,
			"downloadDir":    s.cfg.TargetDir,
			"totalSize":      t.Size,
			"leftUntilDone":  int64(t.Size) - t.Downloaded,
			"uploadedEver":   t.Uploaded,
			"downloadedEver": t.Downloaded,
			"percentDone":    percentDone,
			"rateDownload":   t.DownloadSpeed,
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
func (s *Server) handleTorrentRemove(args json.RawMessage) (interface{}, error) {
	var params struct {
		IDs             []string `json:"ids"`
		DeleteLocalData bool     `json:"delete-local-data"`
	}

	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	for _, hash := range params.IDs {
		transfer, err := s.findTransferByHash(hash)
		if err != nil {
			log.Error("rpc").
				Str("operation", "torrent-remove").
				Str("hash", hash).
				Err(err).
				Msg("Failed to find transfer")
			continue
		}

		// Delete the files of the transfer
		if err := s.client.DeleteFile(transfer.FileID); err != nil {
			log.Error("rpc").
				Str("operation", "torrent-remove").
				Str("hash", hash).
				Int64("transfer_id", transfer.ID).
				Err(err).
				Msg("Failed to delete transfer files")
		}

		if err := s.client.DeleteTransfer(transfer.ID); err != nil {
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
	}

	return struct{}{}, nil
}
