package download

import (
	"context"
	"time"

	grab "github.com/cavaliergopher/grab/v3"
	"github.com/elsbrock/plundrio/internal/log"
)

// monitorGrabDownloadProgress starts a goroutine to monitor and log download progress from grab
func (m *Manager) monitorGrabDownloadProgress(ctx context.Context, state *DownloadState, resp *grab.Response, done chan struct{}, progressTicker *time.Ticker) {
	fileSize := resp.Size()

	go func() {
		log.Info("download").
			Str("file_name", state.Name).
			Float64("size_mb", float64(fileSize)/1024/1024).
			Msg("Starting download")

		// Get transfer context to update downloaded bytes
		transferCtx, exists := m.coordinator.GetTransferContext(state.TransferID)
		if !exists {
			log.Error("download").
				Str("file_name", state.Name).
				Int64("transfer_id", state.TransferID).
				Msg("Transfer context not found during download")
		}

		for {
			select {
			case <-progressTicker.C:
				totalSize := resp.Size()
				if totalSize > 0 {
					state.mu.Lock()
					bytesComplete := resp.BytesComplete()
					bytesDelta := bytesComplete - state.downloaded
					state.downloaded = bytesComplete
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
					speedMBps := downloadedMB / elapsed
					eta := time.Until(state.ETA).Round(time.Second)
					state.LastProgress = time.Now()
					state.mu.Unlock()

					// Update transfer context with downloaded bytes if it exists
					if exists && bytesDelta > 0 {
						transferCtx.DownloadedSize += bytesDelta
						transferCtx.LocalSpeed = speedMBps * 1024 * 1024
						transferCtx.LocalETA = state.ETA

						log.Debug("download").
							Str("file_name", state.Name).
							Int64("transfer_id", state.TransferID).
							Int64("bytes_delta", bytesDelta).
							Int64("transfer_downloaded", transferCtx.DownloadedSize).
							Int64("transfer_total", transferCtx.TotalSize).
							Msg("Updated transfer downloaded bytes")
					}

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
	}()
}
