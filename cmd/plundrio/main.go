package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/elsbrock/putioarr/internal/api"
	"github.com/elsbrock/putioarr/internal/config"
	"github.com/elsbrock/putioarr/internal/download"
	"github.com/elsbrock/putioarr/internal/server"
)

func main() {
	// Parse command line flags
	targetDir := flag.String("target", "", "Target directory for downloads (required)")
	putioFolder := flag.String("folder", "plundrio", "Put.io folder name (will be created if doesn't exist)")
	oauthToken := flag.String("token", "", "Put.io OAuth token (required)")
	listenAddr := flag.String("listen", ":9091", "Address to listen for transmission-rpc requests")
	workerCount := flag.Int("workers", 4, "Number of concurrent download workers")
	earlyFileDelete := flag.Bool("early-delete", false, "Delete files from Put.io before download completes")
	flag.Parse()

	// Validate required flags
	if *targetDir == "" || *putioFolder == "" || *oauthToken == "" {
		log.Println("Error: not all required flags were provided")
		flag.Usage()
		os.Exit(1)
	}

	// Ensure target directory exists
	if err := os.MkdirAll(*targetDir, 0755); err != nil {
		log.Fatalf("Failed to create target directory: %v", err)
	}

	// Initialize configuration
	cfg := &config.Config{
		TargetDir:          *targetDir,
		PutioFolder:        *putioFolder,
		OAuthToken:         *oauthToken,
		ListenAddr:         *listenAddr,
		WorkerCount:        *workerCount,
		EarlyFileDelete:    *earlyFileDelete,
	}

	// Initialize Put.io API client
	client := api.NewClient(cfg.OAuthToken)

	// Authenticate and get account info
	log.Println("Authenticating with Put.io...")
	if err := client.Authenticate(); err != nil {
		log.Fatalf("Failed to authenticate with Put.io: %v", err)
	}
	log.Println("Authentication successful")

	// Create/get folder ID
	log.Printf("Setting up Put.io folder '%s'...", cfg.PutioFolder)
	folderID, err := client.EnsureFolder(cfg.PutioFolder)
	if err != nil {
		log.Fatalf("Failed to create/get folder: %v", err)
	}
	cfg.FolderID = folderID
	log.Printf("Using Put.io folder ID: %d", folderID)

	// Initialize download manager
	dlManager := download.New(cfg, client)
	dlManager.Start()
	defer dlManager.Stop()
	log.Println("Download manager started")

	// Initialize and start RPC server
	srv := server.New(cfg, client, dlManager)
	go func() {
		log.Printf("Starting transmission-rpc server on %s", cfg.ListenAddr)
		if err := srv.Start(); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigChan
	log.Printf("Received signal %v, shutting down...", sig)

	// Cleanup and exit
	if err := srv.Stop(); err != nil {
		log.Printf("Error stopping server: %v", err)
	}
}
