package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/elsbrock/plundrio/internal/api"
	"github.com/elsbrock/plundrio/internal/config"
	"github.com/elsbrock/plundrio/internal/download"
	"github.com/elsbrock/plundrio/internal/log"
	"github.com/elsbrock/plundrio/internal/server"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	version = "dev"
)

var rootCmd = &cobra.Command{
	Use:     "plundrio",
	Short:   "Put.io automation tool",
	Version: version,
}

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the download manager",
	Run: func(cmd *cobra.Command, args []string) {
		// Initialize Viper
		viper.SetEnvPrefix("PLDR")
		viper.AutomaticEnv()

		configFile, _ := cmd.Flags().GetString("config")
		if configFile != "" {
			viper.SetConfigFile(configFile)
			if err := viper.ReadInConfig(); err != nil {
				log.Fatal("config").Str("file", configFile).Err(err).Msg("Error reading config file")
			}
			log.Info("config").Str("file", viper.ConfigFileUsed()).Msg("Using config file")
		}

		// Bind flags to Viper
		viper.BindPFlags(cmd.Flags())

		// Set log level from env/config/flag (in that order)
		logLevel := viper.GetString("log-level")
		if logLevel != "" {
			log.SetLevel(log.LogLevel(logLevel))
		}

		log.Debug("startup").
			Str("version", version).
			Str("log_level", logLevel).
			Msg("Starting plundrio")

		// Get configuration values from viper (which checks env vars, config file, and flags)
		targetDir := viper.GetString("target")
		putioFolder := strings.ToLower(viper.GetString("folder"))
		oauthToken := viper.GetString("token")
		listenAddr := viper.GetString("listen")
		workerCount := viper.GetInt("workers")
		transferCheckInterval := viper.GetString("transfer-check-interval")
		downloadStallTimeout := viper.GetString("download-stall-timeout")
		seedingTimeThreshold := viper.GetString("seeding-time-threshold")
		maxRetryAttempts := viper.GetInt("max-retry-attempts")

		log.Debug("config").
			Str("target_dir", targetDir).
			Str("putio_folder", putioFolder).
			Str("listen_addr", listenAddr).
			Int("workers", workerCount).
			Str("transfer_check_interval", transferCheckInterval).
			Str("download_stall_timeout", downloadStallTimeout).
			Str("seeding_time_threshold", seedingTimeThreshold).
			Int("max_retry_attempts", maxRetryAttempts).
			Msg("Configuration loaded")

		// Validate required configuration values
		// Security warning for token in config file
		if viper.ConfigFileUsed() != "" && viper.IsSet("token") {
			log.Warn("security").
				Str("file", viper.ConfigFileUsed()).
				Msg("OAuth token found in config file - consider using environment variable PLDR_TOKEN instead")
		}

		if targetDir == "" || putioFolder == "" || oauthToken == "" {
			log.Error("config").Msg("Not all required configuration values were provided")
			cmd.Usage()
			os.Exit(1)
		}

		// Verify target directory exists
		stat, err := os.Stat(targetDir)
		if err != nil {
			if os.IsNotExist(err) {
				log.Fatal("config").Str("dir", targetDir).Msg("Target directory does not exist")
			}
			log.Fatal("config").Str("dir", targetDir).Err(err).Msg("Error checking target directory")
		}
		if !stat.IsDir() {
			log.Fatal("config").Str("dir", targetDir).Msg("Target path is not a directory")
		}

		// Initialize configuration
		cfg := &config.Config{
			TargetDir:             targetDir,
			PutioFolder:           putioFolder,
			OAuthToken:            oauthToken,
			ListenAddr:            listenAddr,
			WorkerCount:           workerCount,
			LogLevel:              logLevel,
			TransferCheckInterval: transferCheckInterval,
			DownloadStallTimeout:  downloadStallTimeout,
			SeedingTimeThreshold:  seedingTimeThreshold,
			MaxRetryAttempts:      maxRetryAttempts,
		}

		// Initialize Put.io API client
		client := api.NewClient(cfg.OAuthToken)

		// Authenticate and get account info
		log.Info("auth").Msg("Authenticating with Put.io...")
		if err := client.Authenticate(); err != nil {
			log.Fatal("auth").Err(err).Msg("Failed to authenticate with Put.io")
		}
		log.Info("auth").Msg("Authentication successful")

		// Create/get folder ID
		log.Info("setup").Str("folder", cfg.PutioFolder).Msg("Setting up Put.io folder")
		folderID, err := client.EnsureFolder(cfg.PutioFolder)
		if err != nil {
			log.Fatal("setup").Str("folder", cfg.PutioFolder).Err(err).Msg("Failed to create/get folder")
		}
		cfg.FolderID = folderID
		log.Info("setup").
			Str("folder", cfg.PutioFolder).
			Int64("folder_id", folderID).
			Msg("Using Put.io folder")

		// Initialize download manager
		dlManager := download.New(cfg, client)
		dlManager.Start()
		defer dlManager.Stop()
		log.Info("manager").
			Int("workers", cfg.WorkerCount).
			Msg("Download manager started")

		// Initialize and start RPC server
		srv := server.New(cfg, client, dlManager)
		go func() {
			log.Info("server").
				Str("addr", cfg.ListenAddr).
				Msg("Starting transmission-rpc server")
			if err := srv.Start(); err != nil {
				log.Fatal("server").Err(err).Msg("Server error")
			}
		}()

		// Wait for interrupt signal
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigChan
		log.Info("shutdown").
			Str("signal", sig.String()).
			Msg("Received signal, shutting down...")

		// Cleanup and exit
		log.Info("shutdown").Msg("Stopping download manager...")
		dlManager.Stop()

		log.Info("shutdown").Msg("Stopping server...")
		if err := srv.Stop(); err != nil {
			log.Error("shutdown").Err(err).Msg("Error stopping server")
		}
	},
}

var generateConfigCmd = &cobra.Command{
	Use:   "generate-config",
	Short: "Generate sample configuration file",
	Run: func(cmd *cobra.Command, args []string) {
		cfg := `# Plundrio configuration
# Save as ~/.plundrio.yaml or specify with --config

# Basic configuration
target: /path/to/downloads	# Target directory for downloads
folder: "plundrio"					# Folder name on Put.io
token: "" 									# Get a token with get-token
listen: ":9091"							# Transmission RPC server address
workers: 4									# Number of download workers
log_level: "info"					  # Log level (trace,debug,info,warn,error,fatal,panic,none,pretty)

# Advanced download configuration
transfer_check_interval: "30s"  # How often to check for new transfers
download_stall_timeout: "2m"    # How long a download can stall before being cancelled
seeding_time_threshold: "24h"   # How long a transfer should seed before being cancelled
max_retry_attempts: 3           # Maximum number of times to retry a failed transfer

# Environment variables:
# PLDR_TARGET, PLDR_FOLDER, PLDR_TOKEN, PLDR_LISTEN, PLDR_WORKERS, PLDR_LOG_LEVEL
# PLDR_TRANSFER_CHECK_INTERVAL, PLDR_DOWNLOAD_STALL_TIMEOUT, PLDR_SEEDING_TIME_THRESHOLD, PLDR_MAX_RETRY_ATTEMPTS
`

		outputPath := "plundrio-config.yaml"
		if len(args) > 0 {
			outputPath = args[0]
		}

		log.Debug("config").
			Str("path", outputPath).
			Msg("Generating sample configuration")

		if err := os.WriteFile(outputPath, []byte(cfg), 0644); err != nil {
			log.Fatal("config").
				Str("file", outputPath).
				Err(err).
				Msg("Failed to write config file")
		}
		log.Info("config").
			Str("file", outputPath).
			Msg("Sample config created")
	},
}

var getTokenCmd = &cobra.Command{
	Use:   "get-token",
	Short: "Get OAuth token using device code flow",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		log.Debug("auth").Msg("Starting OAuth device code flow")

		// Step 1: Get OOB code from Put.io
		resp, err := http.Get("https://api.put.io/v2/oauth2/oob/code?app_id=3270")
		if err != nil {
			log.Fatal("auth").Err(err).Msg("Failed to get OOB code")
		}
		defer resp.Body.Close()

		var codeResponse struct {
			Code      string `json:"code"`
			QrCodeURL string `json:"qr_code_url"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&codeResponse); err != nil {
			log.Fatal("auth").Err(err).Msg("Failed to decode code response")
		}

		log.Info("auth").
			Str("code", codeResponse.Code).
			Str("qr_url", codeResponse.QrCodeURL).
			Msg("Visit put.io/link and enter code")
		log.Info("auth").Msg("Waiting for authorization...")

		// Step 2: Poll for token
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				log.Fatal("auth").Msg("Authorization timed out")
			case <-ticker.C:
				tokenResp, err := http.Get("https://api.put.io/v2/oauth2/oob/code/" + codeResponse.Code)
				if err != nil {
					log.Fatal("auth").Err(err).Msg("Failed to check authorization status")
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
					log.Info("auth").
						Str("token", tokenResult.OAuthToken).
						Msg("Successfully obtained access token")
					return
				}

				log.Debug("auth").
					Str("status", tokenResult.Status).
					Msg("Polling for authorization")
			}
		}
	},
}

func init() {
	// Run command flags
	runCmd.Flags().String("config", "", "Config file (default $HOME/.plundrio.yaml)")
	runCmd.Flags().StringP("target", "t", "", "Target directory for downloads (required)")
	runCmd.Flags().StringP("folder", "f", "plundrio", "Put.io folder name")
	runCmd.Flags().StringP("token", "k", "", "Put.io OAuth token (required)")
	runCmd.Flags().StringP("listen", "l", ":9091", "Listen address")
	runCmd.Flags().IntP("workers", "w", 4, "Number of workers")
	runCmd.Flags().String("log-level", "", "Log level (trace,debug,info,warn,error,fatal,none,pretty)")

	// Advanced download configuration flags
	runCmd.Flags().String("transfer-check-interval", "30s", "How often to check for new transfers")
	runCmd.Flags().String("download-stall-timeout", "2m", "How long a download can stall before being cancelled")
	runCmd.Flags().String("seeding-time-threshold", "24h", "How long a transfer should seed before being cancelled")
	runCmd.Flags().Int("max-retry-attempts", 3, "Maximum number of times to retry a failed transfer")

	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(getTokenCmd)
	rootCmd.AddCommand(generateConfigCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		log.Fatal("main").Err(err).Msg("Command execution failed")
	}
}
