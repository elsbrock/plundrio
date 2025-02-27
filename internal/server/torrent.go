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

	transfers, err := s.client.GetTransfers()
	if err != nil {
		return nil, fmt.Errorf("failed to get transfers: %w", err)
	}

	// Convert Put.io transfers to transmission format
	torrents := make([]map[string]interface{}, 0, len(transfers))
	for _, t := range transfers {
		// Only include transfers in our folder
		if t.SaveParentID != s.cfg.FolderID {
			continue
		}

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

		status := s.mapPutioStatus(t.Status)
		torrents = append(torrents, map[string]interface{}{
			"id":             t.ID,
			"hashString":     t.Hash,
			"name":           t.Name,
			"status":         status,
			"downloadDir":    s.cfg.TargetDir,
			"totalSize":      t.Size,
			"leftUntilDone":  int64(t.Size) - t.Downloaded,
			"uploadedEver":   t.Uploaded,
			"downloadedEver": t.Downloaded,
			"percentDone":    float64(t.PercentDone) / 100.0,
			"rateDownload":   t.DownloadSpeed,
			"rateUpload":     t.UploadSpeed,
			"uploadRatio":    float64(t.Uploaded) / float64(t.Size),
			"error":          t.ErrorMessage != "",
			"errorString":    t.ErrorMessage,
		})
	}

	return map[string]interface{}{
		"torrents": torrents,
	}, nil
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
