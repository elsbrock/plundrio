package server

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	_ "net/http/pprof"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/config"
	"github.com/elsbrock/plundrio/internal/download"
	"github.com/elsbrock/plundrio/internal/log"
)

// PutioClient abstracts the put.io API methods used by the RPC server.
type PutioClient interface {
	GetAccountInfo(ctx context.Context) (*putio.AccountInfo, error)
	GetTransfers(ctx context.Context) ([]*putio.Transfer, error)
	UploadFile(ctx context.Context, data []byte, filename string, folderID int64) (string, error)
	AddTransfer(ctx context.Context, magnetLink string, folderID int64) (string, error)
	DeleteFile(ctx context.Context, fileID int64) error
	DeleteTransfer(ctx context.Context, transferID int64) error
}

// DownloadService abstracts the download manager for the RPC server.
type DownloadService interface {
	GetTransfers() []*putio.Transfer
	GetTransferContext(transferID int64) (*download.TransferContext, bool)
	SetCategory(hash, category string)
	GetCategory(hash string) string
	RemoveCategory(hash string)
	Stop()
}

// Server handles transmission-rpc requests
type Server struct {
	cfg          *config.Config
	client       PutioClient
	srv          *http.Server
	quotaTicker  *time.Ticker
	stopChan     chan struct{}
	dlService    DownloadService
	quotaWarning atomic.Bool // tracks if we've already warned about quota
}

// New creates a new RPC server
func New(cfg *config.Config, client PutioClient, dlService DownloadService) *Server {
	return &Server{
		cfg:         cfg,
		client:      client,
		stopChan:    make(chan struct{}),
		dlService:   dlService,
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
	account, err := s.client.GetAccountInfo(context.Background())
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

	// Stop the download service
	s.dlService.Stop()

	if s.srv != nil {
		return s.srv.Close()
	}
	return nil
}
