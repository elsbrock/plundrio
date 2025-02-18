package server

import (
	"encoding/json"
	"log"
	"net/http"
)

// handleRPC processes transmission-rpc requests
func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method    string          `json:"method"`
		Arguments json.RawMessage `json:"arguments"`
		Tag       interface{}     `json:"tag,omitempty"`
	}

	// Handle GET method for session-get
	if r.Method == http.MethodGet {
		req.Method = "session-get"
	} else if r.Method == http.MethodPost {
		// Parse RPC request for POST method
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("Failed to decode request from %s: %v", r.RemoteAddr, err)
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}
	} else {
		log.Printf("Invalid method %s from %s", r.Method, r.RemoteAddr)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
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
