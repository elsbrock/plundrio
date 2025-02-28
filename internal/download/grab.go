package download

import (
	"context"
	"net/http"
	"time"

	grab "github.com/cavaliergopher/grab/v3"
	"github.com/elsbrock/plundrio/internal/log"
)

// createGrabClient creates a configured grab client for downloads
func (m *Manager) createGrabClient() *grab.Client {
	log.Debug("download").
		Dur("idle_timeout", m.dlConfig.IdleConnectionTimeout).
		Dur("header_timeout", m.dlConfig.DownloadHeaderTimeout).
		Msg("Creating download client")

	client := grab.NewClient()
	client.HTTPClient = &http.Client{
		Timeout: 0, // No timeout for large downloads
		Transport: &http.Transport{
			DisableCompression:    true,  // Disable compression for large files
			DisableKeepAlives:     false, // Enable keep-alives
			IdleConnTimeout:       m.dlConfig.IdleConnectionTimeout,
			ResponseHeaderTimeout: m.dlConfig.DownloadHeaderTimeout,
		},
	}

	return client
}

// monitorGrabProgress monitors and logs download progress from grab
func (m *Manager) monitorGrabProgress(ctx context.Context, state *DownloadState, resp *grab.Response, done chan struct{}, progressTicker *time.Ticker) {
	log.Info("download").
		Str("file_name", state.Name).
		Float64("size_mb", float64(resp.Size())/1024/1024).
		Msg("Starting download")

	for {
		select {
		case <-progressTicker.C:
			totalSize := resp.Size()
			if totalSize > 0 {
				state.mu.Lock()
				state.downloaded = resp.BytesComplete()
				state.Progress = resp.Progress()

				// Calculate ETA based on current download rate
				elapsed := time.Since(state.StartTime).Seconds()
				if elapsed > 0 {
					speed := float64(state.downloaded) / elapsed
					remaining := float64(totalSize - state.downloaded)
					if speed > 0 { // Avoid division by zero
						etaSeconds := remaining / speed
						state.ETA = time.Now().Add(time.Duration(etaSeconds) * time.Second)
					}
				}

				downloadedMB := float64(state.downloaded) / 1024 / 1024
				totalMB := float64(totalSize) / 1024 / 1024
				progress := state.Progress * 100
				elapsed = time.Since(state.StartTime).Seconds()
				speedMBps := downloadedMB / elapsed
				eta := time.Until(state.ETA).Round(time.Second)
				state.LastProgress = time.Now()
				state.mu.Unlock()

				log.Info("download").
					Str("file_name", state.Name).
					Float64("progress_percent", progress).
					Float64("downloaded_mb", downloadedMB).
					Float64("total_mb", totalMB).
					Float64("speed_mbps", speedMBps).
					Str("eta", eta.String()).
					Msg("Download progress")
			}
		case <-ctx.Done():
			log.Info("download").
				Str("file_name", state.Name).
				Msg("Download cancelled")
			return
		case <-done:
			return
		}
	}
}
