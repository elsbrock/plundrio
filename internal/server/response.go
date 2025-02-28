package server

import (
	"encoding/json"
	"net/http"

	"github.com/elsbrock/plundrio/internal/log"
)

// sendError sends an error response
func (s *Server) sendError(w http.ResponseWriter, err error) {
	log.Error("server").Msgf("Error processing request: %v", err)

	resp := struct {
		Result  string `json:"result"`
		Message string `json:"message,omitempty"`
	}{
		Result:  "error",
		Message: err.Error(),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Error("server").Msgf("Failed to encode error response: %v", err)
	}
}

// sendResponse sends a success response
func (s *Server) sendResponse(w http.ResponseWriter, tag interface{}, result interface{}) {
	// Create the response structure that matches what the Transmission client expects
	resp := struct {
		Tag       interface{} `json:"tag,omitempty"`
		Result    string      `json:"result"`
		Arguments interface{} `json:"arguments"`
	}{
		Tag:       tag,
		Result:    "success",
		Arguments: result,
	}

	// Log the response for debugging
	respBytes, _ := json.Marshal(resp)
	log.Debug("server").Msgf("Sending response: %s", string(respBytes))

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Transmission-Session-Id", "123") // Ensure session ID is always sent
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Error("server").Msgf("Failed to encode response: %v", err)
	}
}
