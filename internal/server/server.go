package server

import (
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/elsbrock/plundrio/internal/api"
	"github.com/elsbrock/plundrio/internal/config"
	"github.com/elsbrock/plundrio/internal/download"
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
