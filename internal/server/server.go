package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/elsbrock/putioarr/internal/api"
	"github.com/elsbrock/putioarr/internal/config"
	"github.com/elsbrock/putioarr/internal/download"
)

// Server handles transmission-rpc requests
type Server struct {
	cfg          *config.Config
	client       *api.Client
	srv          *http.Server
	transfers    sync.Map // map[string]*putio.Transfer - magnet hash to transfer
	quotaTicker  *time.Ticker
	stopChan     chan struct{}
	dlManager    *download.Manager
	quotaWarning bool // tracks if we've already warned about quota
}

// New creates a new RPC server
func New(cfg *config.Config, client *api.Client, dlManager *download.Manager) *Server {
	return &Server{
		cfg:       cfg,
		client:    client,
		stopChan:  make(chan struct{}),
		dlManager: dlManager,
	}
}

// Start begins listening for RPC requests
func (s *Server) Start() error {
	// Start download manager
	s.dlManager.Start()

	// Check for incomplete downloads
	incompleteDownloads, err := s.dlManager.FindIncompleteDownloads()
	if err != nil {
		log.Printf("Warning: Failed to check for incomplete downloads: %v", err)
	} else if len(incompleteDownloads) > 0 {
		log.Printf("Found %d incomplete downloads to resume", len(incompleteDownloads))
		// Queue incomplete downloads for resumption
		for _, job := range incompleteDownloads {
			// Queue the job for processing by download workers
			s.dlManager.QueueDownload(job)
		}
	}

	// Get and log account info
	account, err := s.client.GetAccountInfo()
	if err != nil {
		log.Printf("Warning: Failed to get account info: %v", err)
	} else {
		log.Printf("Put.io Account: %s", account.Username)
		log.Printf("Storage: %d MB used / %d MB total (Available: %d MB)",
			account.Disk.Used/1024/1024,
			account.Disk.Size/1024/1024,
			account.Disk.Avail/1024/1024)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/transmission/rpc", s.handleRPC)

	s.srv = &http.Server{
		Addr:    s.cfg.ListenAddr,
		Handler: mux,
	}

	// Check initial disk quota
	if overQuota, err := s.checkDiskQuota(); err != nil {
		log.Printf("Warning: Failed to check initial disk quota: %v", err)
	} else if overQuota {
		log.Printf("Warning: Put.io account is over quota on startup")
	}

	// Start quota monitoring (every 15 minutes)
	s.quotaTicker = time.NewTicker(15 * time.Minute)
	go func() {
		for {
			select {
			case <-s.quotaTicker.C:
				if _, err := s.checkDiskQuota(); err != nil {
					log.Printf("Failed to check disk quota: %v", err)
				}
			case <-s.stopChan:
				return
			}
		}
	}()

	log.Printf("Starting transmission-rpc server on %s", s.cfg.ListenAddr)
	return s.srv.ListenAndServe()
}

// Stop gracefully shuts down the server
func (s *Server) Stop() error {
	// Stop quota monitoring
	if s.quotaTicker != nil {
		s.quotaTicker.Stop()
	}
	close(s.stopChan)

	// Stop the download manager
	if s.dlManager != nil {
		s.dlManager.Stop()
	}

	if s.srv != nil {
		return s.srv.Close()
	}
	return nil
}

// handleRPC processes transmission-rpc requests
func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		log.Printf("Invalid method %s from %s", r.Method, r.RemoteAddr)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse RPC request
	var req struct {
		Method    string          `json:"method"`
		Arguments json.RawMessage `json:"arguments"`
		Tag       interface{}     `json:"tag,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("Failed to decode request from %s: %v", r.RemoteAddr, err)
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	// Handle different RPC methods
	var (
		result interface{}
		err    error
	)

	switch req.Method {
	case "torrent-add":
		result, err = s.handleTorrentAdd(req.Arguments)
	case "torrent-get":
		result, err = s.handleTorrentGet(req.Arguments)
	case "torrent-remove":
		result, err = s.handleTorrentRemove(req.Arguments)
	case "session-get":
		result = map[string]interface{}{
			"download-dir":        s.cfg.TargetDir,
			"version":             "2.94", // Transmission version to report
			"rpc-version":         15,     // RPC version to report
			"rpc-version-minimum": 1,
		}
	default:
		// Return empty success for unsupported methods
		result = struct{}{}
	}

	// Send response
	if err != nil {
		s.sendError(w, err)
		return
	}

	s.sendResponse(w, req.Tag, result)
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
	// Get magnet link from either magnetLink or filename field
	var magnetLink string
	if params.MagnetLink != "" {
		magnetLink = params.MagnetLink
	} else if params.Filename != "" && strings.HasPrefix(params.Filename, "magnet:") {
		magnetLink = params.Filename
	} else {
		return nil, fmt.Errorf("only magnet links are supported")
	}

	// Check disk quota before adding transfer
	if overQuota, err := s.checkDiskQuota(); err != nil {
		log.Printf("Warning: Failed to check disk quota before adding transfer: %v", err)
	} else if overQuota {
		return nil, fmt.Errorf("cannot add transfer: Put.io account is over quota")
	}

	// Add transfer to Put.io
	if err := s.client.AddTransfer(magnetLink, s.cfg.FolderID); err != nil {
		return nil, fmt.Errorf("failed to add transfer: %w", err)
	}

	log.Printf("RPC: Added torrent to Put.io: %s", magnetLink)

	// Return success response
	return map[string]interface{}{
		"torrent-added": map[string]interface{}{
			"id":         0, // Put.io doesn't use transmission IDs
			"name":       magnetLink,
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

	return struct{}{}, nil
}

// mapPutioStatus converts Put.io transfer status to transmission status
func (s *Server) mapPutioStatus(status string) int {
	switch status {
	case "IN_QUEUE":
		return 3 // TR_STATUS_DOWNLOAD_WAITING
	case "DOWNLOADING":
		return 4 // TR_STATUS_DOWNLOAD
	case "COMPLETING":
		return 4 // TR_STATUS_DOWNLOAD
	case "SEEDING":
		return 6 // TR_STATUS_SEED
	case "COMPLETED":
		return 6 // TR_STATUS_SEED
	case "ERROR":
		return 0 // TR_STATUS_STOPPED
	default:
		return 0 // TR_STATUS_STOPPED
	}
}

// sendError sends an error response
func (s *Server) sendError(w http.ResponseWriter, err error) {
	log.Printf("Error processing request: %v", err)

	resp := struct {
		Result  string `json:"result"`
		Message string `json:"message,omitempty"`
	}{
		Result:  "error",
		Message: err.Error(),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("Failed to encode error response: %v", err)
	}
}

// checkDiskQuota checks disk usage and handles quota warnings
func (s *Server) checkDiskQuota() (bool, error) {
	account, err := s.client.GetAccountInfo()
	if err != nil {
		return false, fmt.Errorf("failed to check disk quota: %w", err)
	}

	// Calculate usage percentage
	usagePercent := float64(account.Disk.Used) / float64(account.Disk.Size) * 100

	// Consider over quota if usage is above 95%
	isOverQuota := usagePercent >= 95

	if isOverQuota && !s.quotaWarning {
		log.Printf("WARNING: Put.io account is over quota (%.1f%% used)", usagePercent)
		s.quotaWarning = true
	} else if !isOverQuota && s.quotaWarning {
		// Reset warning when usage drops
		s.quotaWarning = false
	}

	return isOverQuota, nil
}

// sendResponse sends a success response
func (s *Server) sendResponse(w http.ResponseWriter, tag interface{}, result interface{}) {
	resp := struct {
		Tag     interface{} `json:"tag,omitempty"`
		Result  string      `json:"result"`
		Message interface{} `json:"arguments,omitempty"`
	}{
		Tag:     tag,
		Result:  "success",
		Message: result,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("Failed to encode response: %v", err)
	}
}
