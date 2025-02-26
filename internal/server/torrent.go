package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/elsbrock/plundrio/internal/log"
)

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
	}

	// Return success response
	return map[string]interface{}{
		"torrent-added": map[string]interface{}{
			"id":         0, // Put.io doesn't use transmission IDs
			"name":       name,
			"hashString": "",
		},
	}, nil
}

// handleTorrentGet processes torrent-get requests
func (s *Server) handleTorrentGet(args json.RawMessage) (interface{}, error) {
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
		status := s.mapPutioStatus(t.Status)
		torrents = append(torrents, map[string]interface{}{
			"id":           t.Hash,
			"name":         t.Name,
			"status":       status,
			"downloadDir":  s.cfg.TargetDir,
			"percentDone":  float64(t.PercentDone) / 100.0,
			"rateDownload": t.DownloadSpeed,
			"rateUpload":   t.UploadSpeed,
			"uploadRatio":  float64(t.Uploaded) / float64(t.Size),
			"error":        t.ErrorMessage != "",
			"errorString":  t.ErrorMessage,
		})
	}

	return map[string]interface{}{
		"torrents": torrents,
	}, nil
}

// handleTorrentRemove processes torrent-remove requests
func (s *Server) handleTorrentRemove(args json.RawMessage) (interface{}, error) {
	var params struct {
		IDs             []int64 `json:"ids"`
		DeleteLocalData bool    `json:"delete-local-data"`
	}

	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	for _, id := range params.IDs {
		if err := s.client.DeleteTransfer(id); err != nil {
			log.Error("rpc").
				Str("operation", "torrent-remove").
				Int64("transfer_id", id).
				Err(err).
				Msg("Failed to delete transfer")
		} else {
			log.Info("rpc").
				Str("operation", "torrent-remove").
				Int64("transfer_id", id).
				Bool("delete_local_data", params.DeleteLocalData).
				Msg("Transfer removed")
		}
	}

	return struct{}{}, nil
}
