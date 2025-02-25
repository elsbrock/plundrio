package server

import (
	"net/http"
	"time"

	_ "net/http/pprof"

	"github.com/elsbrock/plundrio/internal/api"
	"github.com/elsbrock/plundrio/internal/config"
	"github.com/elsbrock/plundrio/internal/download"
	"github.com/elsbrock/plundrio/internal/log"
)

// Server handles transmission-rpc requests
type Server struct {
	cfg          *config.Config
	client       *api.Client
	srv          *http.Server
	quotaTicker  *time.Ticker
	stopChan     chan struct{}
	dlManager    *download.Manager
	quotaWarning bool // tracks if we've already warned about quota
}

// New creates a new RPC server
func New(cfg *config.Config, client *api.Client, dlManager *download.Manager) *Server {
	return &Server{
		cfg:         cfg,
		client:      client,
		stopChan:    make(chan struct{}),
		dlManager:   dlManager,
		quotaTicker: time.NewTicker(15 * time.Minute),
	}
}

// Start begins listening for RPC requests
func (s *Server) Start() error {
	// Initialize server first
	mux := http.NewServeMux()
	mux.HandleFunc("/transmission/rpc", s.handleRPC)

	s.srv = &http.Server{
		Addr:    s.cfg.ListenAddr,
		Handler: mux,
	}

	// Get and log account info
	account, err := s.client.GetAccountInfo()
	if err != nil {
		log.Warn("server").Err(err).Msg("Failed to get account info")
	} else {
		log.Info("server").
			Str("username", account.Username).
			Int64("storage_used_mb", account.Disk.Used/1024/1024).
			Int64("storage_total_mb", account.Disk.Size/1024/1024).
			Int64("storage_available_mb", account.Disk.Avail/1024/1024).
			Msg("Put.io account status")
	}

	// Check initial disk quota
	if overQuota, err := s.checkDiskQuota(); err != nil {
		log.Warn("server").Err(err).Msg("Failed to check initial disk quota")
	} else if overQuota {
		log.Warn("server").Msg("Put.io account is over quota on startup")
	}

	// Start quota monitoring
	go func() {
		for {
			select {
			case <-s.quotaTicker.C:
				if _, err := s.checkDiskQuota(); err != nil {
					log.Error("server").Err(err).Msg("Failed to check disk quota")
				}
			case <-s.stopChan:
				return
			}
		}
	}()

	log.Info("server").Str("addr", s.cfg.ListenAddr).Msg("Starting transmission-rpc server")
	return s.srv.ListenAndServe()
}

// Stop gracefully shuts down the server
func (s *Server) Stop() error {
	s.quotaTicker.Stop()
	close(s.stopChan)

	// Stop the download manager
	s.dlManager.Stop()

	if s.srv != nil {
		return s.srv.Close()
	}
	return nil
}
