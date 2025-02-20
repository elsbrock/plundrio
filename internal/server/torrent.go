package server

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/putdotio/go-putio/putio"
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
	// Get magnet link from either magnetLink or filename field
	var magnetLink string
	if params.MagnetLink != "" {
		magnetLink = params.MagnetLink
	} else if params.Filename != "" && strings.HasPrefix(params.Filename, "magnet:") {
		magnetLink = params.Filename
	} else {
		return nil, fmt.Errorf("only magnet links are supported")
	}

	// Add transfer to Put.io
	if err := s.client.AddTransfer(magnetLink, s.cfg.FolderID); err != nil {
		return nil, fmt.Errorf("failed to add transfer: %w", err)
	}

	log.Printf("RPC: torrent added")

	// Return success response
	return map[string]interface{}{
		"torrent-added": map[string]interface{}{
			"id":         0, // Put.io doesn't use transmission IDs
			"name":       magnetLink,
			"hashString": "",
		},
	}, nil
}

// verifyTransferFiles checks if all files in a transfer exist locally with matching sizes
func (s *Server) verifyTransferFiles(transfer *putio.Transfer) (bool, error) {
	// Get all files in the transfer
	files, err := s.client.GetAllTransferFiles(transfer.FileID)
	if err != nil {
		return false, fmt.Errorf("failed to get transfer files: %w", err)
	}

	// Check each file exists locally with matching size
	for _, file := range files {
		localPath := filepath.Join(s.cfg.TargetDir, file.Name)
		info, err := os.Stat(localPath)
		if err != nil {
			if os.IsNotExist(err) {
				return false, nil // File doesn't exist
			}
			return false, fmt.Errorf("failed to check local file: %w", err)
		}

		if info.Size() != file.Size {
			return false, nil // Size mismatch
		}
	}

	return true, nil // All files exist with matching sizes
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
			"id":           t.ID,
			"name":         t.Name,
			"status":       status,
			"downloadDir":  s.cfg.TargetDir,
			"percentDone":  float64(t.PercentDone) / 100.0,
			"rateDownload": t.DownloadSpeed,
			"rateUpload":   0, // Put.io doesn't provide upload speed
			"uploadRatio":  0, // Put.io doesn't provide ratio
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
			log.Printf("RPC: Failed to delete transfer %d: %v", id, err)
		} else {
			log.Printf("RPC: Removed torrent with ID %d", id)
		}
	}

	log.Printf("RPC: torrent removed")

	return struct{}{}, nil
}
