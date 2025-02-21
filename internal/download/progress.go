package download

import (
	"context"
	"io"
	"log"
	"time"
)

// setupProgressTracking configures progress tracking for the download
func (m *Manager) setupProgressTracking(state *DownloadState, body io.Reader, downloaded *int64, totalSize int64) *progressReader {
	startTime := time.Now()
	bytesThisSession := int64(0)
	reader := &progressReader{
		reader:       body,
		startTime:    startTime,
		initialBytes: *downloaded, // Store initial bytes for speed calculation
		onProgress: func(n int64) {
			*downloaded += n
			bytesThisSession += n
			if totalSize > 0 {
				state.Progress = float64(*downloaded) / float64(totalSize)

				// Calculate ETA based on current download rate
				elapsed := time.Since(startTime).Seconds()
				if elapsed > 0 {
					// Use only bytes downloaded this session for speed
					speed := float64(bytesThisSession) / elapsed
					remaining := float64(totalSize - *downloaded)
					if speed > 0 { // Avoid division by zero
						etaSeconds := remaining / speed
						state.ETA = time.Now().Add(time.Duration(etaSeconds) * time.Second)
					}
				}
			}
			state.LastProgress = time.Now()
		},
	}
	return reader
}

// monitorDownloadProgress starts a goroutine to monitor and log download progress
func (m *Manager) monitorDownloadProgress(ctx context.Context, state *DownloadState, reader *progressReader, totalSize int64, downloaded *int64, done chan struct{}, progressTicker *time.Ticker) {
	go func() {
		log.Printf("Starting download of %s (%.2f MB)", state.Name, float64(totalSize)/1024/1024)
		for {
			select {
			case <-progressTicker.C:
				if totalSize > 0 {
					progress := float64(*downloaded) / float64(totalSize) * 100
					downloadedMB := float64(*downloaded) / 1024 / 1024
					totalMB := float64(totalSize) / 1024 / 1024
					elapsed := time.Since(reader.startTime).Seconds()
					// Calculate speed based on bytes downloaded in this session
					sessionBytes := float64(*downloaded-reader.initialBytes) / 1024 / 1024
					speedMBps := sessionBytes / elapsed
					eta := time.Until(state.ETA).Round(time.Second)
					log.Printf("Downloading %s: %.1f%% (%.1f/%.1f MB) - %.2f MB/s ETA: %s",
						state.Name, progress, downloadedMB, totalMB, speedMBps, eta)
				}
			case <-ctx.Done():
				log.Printf("Download of %s cancelled", state.Name)
				return
			case <-done:
				return
			}
		}
	}()
}

// monitorDownloadStall monitors for stalled downloads
func (m *Manager) monitorDownloadStall(ctx context.Context, state *DownloadState, downloaded *int64, totalSize int64, cancel context.CancelFunc) {
	lastProgress := *downloaded
	lastProgressTime := time.Now()
	go func() {
		for {
			select {
			case <-time.After(5 * time.Second):
				currentDownloaded := *downloaded
				if currentDownloaded == lastProgress && currentDownloaded < totalSize {
					stalledDuration := time.Since(lastProgressTime)
					if stalledDuration > downloadStallTimeout {
						log.Printf("Download %s stalled for over %v, cancelling", state.Name, downloadStallTimeout)
						cancel()
						return
					}
					log.Printf("Warning: Download %s stalled for %v", state.Name, stalledDuration.Round(time.Second))
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
