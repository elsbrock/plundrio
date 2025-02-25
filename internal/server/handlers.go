package server

import (
	"encoding/json"
	"net/http"

	"github.com/elsbrock/plundrio/internal/log"
)

// handleRPC processes transmission-rpc requests
func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	// Check for session ID header
	sessionID := r.Header.Get("X-Transmission-Session-Id")
	if sessionID == "" {
		// Client needs to authenticate - send session ID
		log.Info("rpc").
			Str("client_addr", r.RemoteAddr).
			Msg("Client needs authentication - sending session ID")
		w.Header().Set("X-Transmission-Session-Id", "123") // Using a simple static ID for now
		http.Error(w, "409 Conflict", http.StatusConflict)
		return
	}

	log.Debug("rpc").
		Str("client_addr", r.RemoteAddr).
		Str("session_id", sessionID).
		Str("method", r.Method).
		Msg("Handling RPC request")

	var req struct {
		Method    string          `json:"method"`
		Arguments json.RawMessage `json:"arguments"`
		Tag       interface{}     `json:"tag,omitempty"`
	}

	// Handle GET method for session-get
	if r.Method == http.MethodGet {
		req.Method = "session-get"
		log.Debug("rpc").
			Str("client_addr", r.RemoteAddr).
			Str("method", "GET").
			Msg("GET request converted to session-get")
	} else if r.Method == http.MethodPost {
		// Parse RPC request for POST method
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Error("rpc").
				Str("client_addr", r.RemoteAddr).
				Str("method", "POST").
				Err(err).
				Msg("Failed to decode request")
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}
		log.Debug("rpc").
			Str("client_addr", r.RemoteAddr).
			Str("method", "POST").
			Str("rpc_method", req.Method).
			Msg("Decoded RPC request")
	} else {
		log.Error("rpc").
			Str("client_addr", r.RemoteAddr).
			Str("method", r.Method).
			Msg("Invalid HTTP method")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Handle different RPC methods
	var (
		result interface{}
		err    error
	)

	log.Debug("rpc").
		Str("client_addr", r.RemoteAddr).
		Str("rpc_method", req.Method).
		Msg("Processing RPC method")

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
		log.Debug("rpc").
			Str("client_addr", r.RemoteAddr).
			Str("download_dir", s.cfg.TargetDir).
			Msg("Session information requested")
	default:
		// Return empty success for unsupported methods
		result = struct{}{}
		log.Debug("rpc").
			Str("client_addr", r.RemoteAddr).
			Str("rpc_method", req.Method).
			Msg("Unsupported RPC method called")
	}

	// Send response
	if err != nil {
		s.sendError(w, err)
		return
	}

	log.Debug("rpc").
		Str("client_addr", r.RemoteAddr).
		Str("rpc_method", req.Method).
		Msg("Sending RPC response")

	s.sendResponse(w, req.Tag, result)
}
