package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/log"
)

// findTransferByHash finds a transfer by its hash string
func (s *Server) findTransferByHash(hash string) (*putio.Transfer, error) {
	// First check the cache via RPC handler
	if s.dlManager != nil && s.dlManager.GetRPCHandler() != nil {
		// Try to get the transfer directly from cache
		if cachedTransfer, ok := s.dlManager.GetRPCHandler().GetTransferByHash(hash); ok {
			// Convert cached transfer to putio.Transfer
			return &putio.Transfer{
				ID:             cachedTransfer.ID,
				Hash:           cachedTransfer.Hash,
				Name:           cachedTransfer.Name,
				Status:         cachedTransfer.Status,
				Size:           int(cachedTransfer.Size),
				Downloaded:     cachedTransfer.Downloaded,
				FileID:         cachedTransfer.FileID,
				SecondsSeeding: cachedTransfer.SecondsSeeding,
				ErrorMessage:   cachedTransfer.ErrorMessage,
				Availability:   cachedTransfer.Availability,
				PercentDone:    cachedTransfer.PercentDone,
			}, nil
		}

		// Try to get the transfer ID from the hash
		if transferID, ok := s.dlManager.GetRPCHandler().GetTransferIDByHash(hash); ok {
			// Now try to get the transfer from the API using the ID
			transfers, err := s.client.GetTransfers()
			if err != nil {
				return nil, err
			}
			for _, t := range transfers {
				if t.ID == transferID {
					// Update the hash mapping in case it's missing
					s.dlManager.GetRPCHandler().AddTransfer(t)
					return t, nil
				}
			}
		}
	}

	// Fall back to API call if not found in cache
	transfers, err := s.client.GetTransfers()
	if err != nil {
		return nil, err
	}
	for _, t := range transfers {
		if t.Hash == hash {
			// If we found it, add it to the cache for next time
			if s.dlManager != nil && s.dlManager.GetRPCHandler() != nil {
				s.dlManager.GetRPCHandler().AddTransfer(t)
			}
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

		// We need to get the transfer that was created to add it to the cache
		// Wait a moment for the transfer to be created
		time.Sleep(1 * time.Second)

		// Get all transfers and find the one we just created
		transfers, err := s.client.GetTransfers()
		if err == nil && s.dlManager != nil && s.dlManager.GetRPCHandler() != nil {
			// Find the most recently created transfer
			var newestTransfer *putio.Transfer
			var newestTime time.Time

			for _, t := range transfers {
				if t.CreatedAt != nil && t.CreatedAt.After(newestTime) {
					newestTransfer = t
					newestTime = t.CreatedAt.Time
				}
			}

			// Add the transfer to the cache
			if newestTransfer != nil {
				s.dlManager.GetRPCHandler().AddTransfer(newestTransfer)
				log.Info("rpc").
					Str("operation", "torrent-add").
					Int64("transfer_id", newestTransfer.ID).
					Str("hash", newestTransfer.Hash).
					Msg("Added new transfer to cache")
			}
		}
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
		transfer, err := s.client.AddTransferAndReturn(name, s.cfg.FolderID)
		if err != nil {
			return nil, fmt.Errorf("failed to add transfer: %w", err)
		}

		// Add to cache if we have an RPC handler
		if s.dlManager != nil && s.dlManager.GetRPCHandler() != nil {
			s.dlManager.GetRPCHandler().AddTransfer(transfer)
		}

		log.Info("rpc").
			Str("operation", "torrent-add").
			Str("type", "magnet").
			Str("magnet", name).
			Int64("folder_id", s.cfg.FolderID).
			Int64("transfer_id", transfer.ID).
			Str("hash", transfer.Hash).
			Msg("Magnet link added")

		// Return success response
		return map[string]interface{}{
			"torrent-added": map[string]interface{}{
				"id":         transfer.ID,
				"hashString": transfer.Hash,
				"name":       transfer.Name,
			},
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

	var transfers []*putio.Transfer

	// Check if we have an RPC handler to get transfers from cache
	if s.dlManager != nil && s.dlManager.GetRPCHandler() != nil {
		// Get transfers from cache
		cachedTransfers := s.dlManager.GetRPCHandler().GetAllTransfers()

		// Convert cached transfers to putio.Transfer objects
		for _, ct := range cachedTransfers {
			transfers = append(transfers, &putio.Transfer{
				ID:             ct.ID,
				Hash:           ct.Hash,
				Name:           ct.Name,
				Status:         ct.Status,
				Size:           int(ct.Size),
				Downloaded:     ct.Downloaded,
				FileID:         ct.FileID,
				SecondsSeeding: ct.SecondsSeeding,
				ErrorMessage:   ct.ErrorMessage,
				Availability:   ct.Availability,
				PercentDone:    ct.PercentDone,
			})
		}

		log.Debug("rpc").
			Str("operation", "torrent-get").
			Int("cached_transfers", len(cachedTransfers)).
			Msg("Using cached transfers from RPC handler")
	} else {
		// Fall back to processor if RPC handler is not available
		processor := s.dlManager.GetTransferProcessor()

		// Check if processor is nil
		if processor == nil {
			log.Error("rpc").
				Str("operation", "torrent-get").
				Msg("Transfer processor is nil")
			return map[string]interface{}{
				"torrents": []map[string]interface{}{},
			}, nil
		}

		// Log processor details
		log.Debug("rpc").
			Str("operation", "torrent-get").
			Msg("Using transfer processor")

		transfers = processor.GetTransfers()
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

		// Calculate combined progress
		var percentDone float64
		var status int
		var leftUntilDone int64

		// Check if we have an RPC handler to calculate progress
		if s.dlManager.GetRPCHandler() != nil {
			// Use the progress tracker to calculate progress
			calculatedProgress, remainingBytes := s.dlManager.GetRPCHandler().GetProgress(t.ID, t)
			percentDone = calculatedProgress / 100.0 // Convert from 0-100 to 0-1
			leftUntilDone = remainingBytes

			// Check if the transfer is processed
			isProcessed := s.dlManager.GetRPCHandler().IsTransferProcessed(t.ID)

			// Get the transfer context for additional information
			ctx, exists := s.dlManager.GetRPCHandler().GetTransferContext(t.ID)

			if isProcessed {
				// For transfers that have been processed locally, show as 100% complete
				percentDone = 1.0 // 100%
				leftUntilDone = 0 // Nothing left to download
				status = 6        // TR_STATUS_SEED (completed/seeding)
			} else if exists && ctx.TotalFiles > 0 {
				// If the transfer is being processed but not yet complete
				if ctx.CompletedFiles+ctx.FailedFiles >= ctx.TotalFiles && ctx.FailedFiles == 0 {
					// All files completed, no failures
					status = 6 // TR_STATUS_SEED (completed/seeding)
				} else {
					// Still downloading
					status = 4 // TR_STATUS_DOWNLOAD
				}
			} else {
				// Use the put.io status
				status = s.mapPutioStatus(t.Status)
			}

			log.Debug("rpc").
				Str("operation", "torrent-get").
				Int64("id", t.ID).
				Str("name", t.Name).
				Float64("progress", percentDone*100).
				Int64("left_until_done", leftUntilDone).
				Bool("processed", isProcessed).
				Msg("Calculated progress using RPC handler")
		} else if t.Status == "COMPLETED" || t.Status == "SEEDING" {
			// For transfers that are completed on put.io but have no corresponding entry in the processor
			// (i.e., already downloaded), show as 100% complete with status "downloaded"
			percentDone = 1.0 // 100%
			leftUntilDone = 0 // Nothing left to download
			status = 6        // TR_STATUS_SEED (completed/seeding)
		} else {
			// For other transfers not being processed, just use put.io progress (0-50%)
			putioProgress := float64(t.PercentDone) / 200.0 // Maps 0-100 to 0-0.5
			percentDone = putioProgress

			// Calculate bytes left on Put.io side only
			leftUntilDone = int64(float64(t.Size) * (1.0 - float64(t.PercentDone)/100.0))

			status = s.mapPutioStatus(t.Status)

			log.Debug("rpc").
				Str("operation", "torrent-get").
				Int64("id", t.ID).
				Str("name", t.Name).
				Float64("putio_progress", putioProgress*100).
				Float64("combined_progress", percentDone*100).
				Int64("left_until_done", leftUntilDone).
				Msg("Calculated progress for transfer without context")
		}

		torrentInfo := map[string]interface{}{
			"id":             t.ID,
			"hashString":     t.Hash,
			"name":           t.Name,
			"eta":            t.EstimatedTime,
			"status":         status,
			"downloadDir":    s.cfg.TargetDir,
			"totalSize":      t.Size,
			"leftUntilDone":  leftUntilDone,
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
			// Remove from cache if we have an RPC handler
			if s.dlManager != nil && s.dlManager.GetRPCHandler() != nil {
				s.dlManager.GetRPCHandler().RemoveTransfer(hash)
			}

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
