package server

import (
	"encoding/json"
	"log"
	"net/http"
)

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

// sendResponse sends a success response
func (s *Server) sendResponse(w http.ResponseWriter, tag interface{}, result interface{}) {
	resp := struct {
		Tag       interface{} `json:"tag,omitempty"`
		Result    string      `json:"result"`
		Arguments interface{} `json:"arguments,omitempty"`
	}{
		Tag:       tag,
		Result:    "success",
		Arguments: result,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("Failed to encode response: %v", err)
	}
}
