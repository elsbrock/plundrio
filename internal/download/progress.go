package download

import (
	"context"
	"io"
	"time"

	"github.com/elsbrock/plundrio/internal/log"
)

// setupProgressTracking configures progress tracking for the download
func (m *Manager) setupProgressTracking(state *DownloadState, body io.Reader, totalSize int64) *progressReader {
	startTime := time.Now()
	bytesThisSession := int64(0)

	state.mu.Lock()
	initialBytes := state.downloaded
	state.mu.Unlock()

	reader := &progressReader{
		reader:       body,
		startTime:    startTime,
		initialBytes: initialBytes,
		onProgress: func(n int64) {
			state.mu.Lock()
			state.downloaded += n
			bytesThisSession += n
			if totalSize > 0 {
				state.Progress = float64(state.downloaded) / float64(totalSize)

				// Calculate ETA based on current download rate
				elapsed := time.Since(startTime).Seconds()
				if elapsed > 0 {
					// Use only bytes downloaded this session for speed
					speed := float64(bytesThisSession) / elapsed
					remaining := float64(totalSize - state.downloaded)
					if speed > 0 { // Avoid division by zero
						etaSeconds := remaining / speed
						state.ETA = time.Now().Add(time.Duration(etaSeconds) * time.Second)
					}
				}
			}
			state.LastProgress = time.Now()
			state.mu.Unlock()
		},
	}
	return reader
}

// monitorDownloadProgress starts a goroutine to monitor and log download progress
func (m *Manager) monitorDownloadProgress(ctx context.Context, state *DownloadState, reader *progressReader, totalSize int64, done chan struct{}, progressTicker *time.Ticker) {
	go func() {
		log.Info("download").
			Str("file_name", state.Name).
			Float64("size_mb", float64(totalSize)/1024/1024).
			Msg("Starting download")

		for {
			select {
			case <-progressTicker.C:
				if totalSize > 0 {
					state.mu.Lock()
					downloadedBytes := state.downloaded

					progress := float64(downloadedBytes) / float64(totalSize) * 100
					downloadedMB := float64(downloadedBytes) / 1024 / 1024
					totalMB := float64(totalSize) / 1024 / 1024
					elapsed := time.Since(reader.startTime).Seconds()
					// Calculate speed based on bytes downloaded in this session
					sessionBytes := float64(downloadedBytes-reader.initialBytes) / 1024 / 1024
					speedMBps := sessionBytes / elapsed

					eta := time.Until(state.ETA).Round(time.Second)
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
	}()
}

// monitorDownloadStall monitors for stalled downloads
func (m *Manager) monitorDownloadStall(ctx context.Context, state *DownloadState, totalSize int64, cancel context.CancelFunc) {
	state.mu.Lock()
	lastProgress := state.downloaded
	state.mu.Unlock()
	lastProgressTime := time.Now()

	go func() {
		for {
			select {
			case <-time.After(5 * time.Second):
				state.mu.Lock()
				currentDownloaded := state.downloaded
				state.mu.Unlock()

				if currentDownloaded == lastProgress && currentDownloaded < totalSize {
					stalledDuration := time.Since(lastProgressTime)
					if stalledDuration > m.dlConfig.DownloadStallTimeout {
						log.Error("download").
							Str("file_name", state.Name).
							Dur("stalled_duration", stalledDuration).
							Dur("timeout", m.dlConfig.DownloadStallTimeout).
							Msg("Download stalled, cancelling")

						// Create a stalled download error
						stalledErr := NewDownloadStalledError(state.Name, stalledDuration)
						log.Error("download").
							Str("file_name", state.Name).
							Err(stalledErr).
							Msg("Download error")
						cancel()
						return
					}
					log.Warn("download").
						Str("file_name", state.Name).
						Dur("stalled_duration", stalledDuration.Round(time.Second)).
						Msg("Download stalled")
				} else {
					lastProgress = currentDownloaded
					lastProgressTime = time.Now()
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}
