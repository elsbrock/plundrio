package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
	"encoding/json"
	"net/http"

	"github.com/elsbrock/putioarr/internal/api"
	"github.com/elsbrock/putioarr/internal/config"
	"github.com/elsbrock/putioarr/internal/download"
	"github.com/elsbrock/putioarr/internal/server"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "putioarr",
	Short: "Put.io automation tool",
}

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the download manager",
	Run: func(cmd *cobra.Command, args []string) {
		// Get flag values
		targetDir, _ := cmd.Flags().GetString("target")
		putioFolder, _ := cmd.Flags().GetString("folder")
		oauthToken, _ := cmd.Flags().GetString("token")
		listenAddr, _ := cmd.Flags().GetString("listen")
		workerCount, _ := cmd.Flags().GetInt("workers")
		earlyFileDelete, _ := cmd.Flags().GetBool("early-delete")

		// Validate required flags
		if targetDir == "" || putioFolder == "" || oauthToken == "" {
			log.Println("Error: not all required flags were provided")
			cmd.Usage()
			os.Exit(1)
		}

		// Verify target directory exists
		stat, err := os.Stat(targetDir)
		if err != nil {
			if os.IsNotExist(err) {
				log.Fatalf("Target directory does not exist: %s", targetDir)
			}
			log.Fatalf("Error checking target directory: %v", err)
		}
		if !stat.IsDir() {
			log.Fatalf("Target path is not a directory: %s", targetDir)
		}

		// Initialize configuration
		cfg := &config.Config{
			TargetDir:       targetDir,
			PutioFolder:     putioFolder,
			OAuthToken:      oauthToken,
			ListenAddr:      listenAddr,
			WorkerCount:     workerCount,
			EarlyFileDelete: earlyFileDelete,
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
	},
}

var getTokenCmd = &cobra.Command{
	Use:   "get-token",
	Short: "Get OAuth token using device code flow",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		// Step 1: Get OOB code from Put.io
		resp, err := http.Get("https://api.put.io/v2/oauth2/oob/code?app_id=3270")
		if err != nil {
			log.Fatal("Failed to get OOB code:", err)
		}
		defer resp.Body.Close()

		var codeResponse struct {
			Code      string `json:"code"`
			QrCodeURL string `json:"qr_code_url"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&codeResponse); err != nil {
			log.Fatal("Failed to decode code response:", err)
		}

		log.Printf("Visit put.io/link and enter code: %s\n", codeResponse.Code)
		log.Println("Waiting for authorization...")

		// Step 2: Poll for token
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				log.Fatal("Authorization timed out")
			case <-ticker.C:
				tokenResp, err := http.Get("https://api.put.io/v2/oauth2/oob/code/" + codeResponse.Code)
				if err != nil {
					log.Fatal("Failed to check authorization status:", err)
				}

				var tokenResult struct {
					OAuthToken string `json:"oauth_token"`
					Status     string `json:"status"`
				}
				if err := json.NewDecoder(tokenResp.Body).Decode(&tokenResult); err != nil {
					tokenResp.Body.Close()
					continue
				}
				tokenResp.Body.Close()

				if tokenResult.Status == "OK" && tokenResult.OAuthToken != "" {
					log.Printf("Successfully obtained access token: %s", tokenResult.OAuthToken)
					return
				}
			}
		}
	},
}

func init() {
	// Run command flags
	runCmd.Flags().StringP("target", "t", "", "Target directory for downloads (required)")
	runCmd.Flags().StringP("folder", "f", "plundrio", "Put.io folder name")
	runCmd.Flags().StringP("token", "k", "", "Put.io OAuth token (required)")
	runCmd.Flags().StringP("listen", "l", ":9091", "Listen address")
	runCmd.Flags().IntP("workers", "w", 4, "Number of workers")
	runCmd.Flags().BoolP("early-delete", "e", false, "Enable early file deletion")

	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(getTokenCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		log.Fatal(err)
	}
}
