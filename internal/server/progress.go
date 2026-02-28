package server

import (
	"time"

	"github.com/elsbrock/plundrio/internal/download"
)

// Transmission status constants
const (
	trStatusStopped         = 0
	trStatusDownloadWaiting = 3
	trStatusDownload        = 4
	trStatusSeed            = 6
)

// progressInput holds the data needed to calculate transfer progress.
type progressInput struct {
	// Put.io side
	PutioPercentDone int    // 0–100
	PutioStatus      string // e.g. "DOWNLOADING", "COMPLETED", "SEEDING"
	PutioSize        int    // total torrent size in bytes

	// Local side (nil when no transfer context exists)
	TransferCtx *download.TransferContext
}

// progressResult contains the calculated progress values.
type progressResult struct {
	PercentDone   float64   // 0.0–1.0
	Status        int       // Transmission status code
	LeftUntilDone int64     // bytes remaining
	LocalETA      time.Time // local ETA override (zero if not applicable)
	LocalSpeed    float64   // local download speed override in bytes/sec (0 if not applicable)
}

// calculateProgress computes the combined progress for a transfer.
//
// Progress is split 50/50:
//   - Put.io downloading the torrent (0–50%)
//   - Local download from Put.io (50–100%)
//
// When a transfer context exists it indicates the transfer is actively being
// tracked by the download manager. Otherwise we rely solely on the Put.io
// transfer metadata.
func calculateProgress(in progressInput) progressResult {
	// When we have a transfer context with files, calculate the 50/50 split.
	if in.TransferCtx != nil && in.TransferCtx.TotalFiles > 0 {
		return calculateProgressWithContext(in)
	}

	// Completed/seeding on Put.io without local context → already done.
	if in.PutioStatus == "COMPLETED" || in.PutioStatus == "SEEDING" {
		return progressResult{
			PercentDone:   1.0,
			LeftUntilDone: 0,
			Status:        trStatusSeed,
		}
	}

	// No context — put.io only progress (0–50%).
	putioProgress := float64(in.PutioPercentDone) / 200.0
	leftUntilDone := int64(float64(in.PutioSize) * (1.0 - float64(in.PutioPercentDone)/100.0))

	return progressResult{
		PercentDone:   putioProgress,
		LeftUntilDone: leftUntilDone,
		Status:        mapPutioStatusValue(in.PutioStatus),
	}
}

// calculateProgressWithContext handles the case where we have a local transfer context.
func calculateProgressWithContext(in progressInput) progressResult {
	ctx := in.TransferCtx

	downloadedSize, totalSize, completedFiles, _ := ctx.GetProgress()
	totalFiles := ctx.TotalFiles // write-once, safe without lock
	state := ctx.GetState()
	localSpeed, localETA := ctx.GetLocalProgress()

	// Put.io progress (0–50%)
	putioProgress := float64(in.PutioPercentDone) / 200.0

	// Local download progress (0–50%)
	var localProgress float64
	if totalSize > 0 {
		localProgress = float64(downloadedSize) / float64(totalSize) * 0.5
	} else if totalFiles > 0 {
		localProgress = float64(completedFiles) / float64(totalFiles) * 0.5
	}

	percentDone := putioProgress + localProgress

	// Bytes left on Put.io side
	putioLeftBytes := int64(float64(in.PutioSize) * (1.0 - float64(in.PutioPercentDone)/100.0))
	// Bytes left on local side
	localLeftBytes := totalSize - downloadedSize
	leftUntilDone := putioLeftBytes + localLeftBytes
	if leftUntilDone < 0 {
		leftUntilDone = 0
	}

	var status int
	switch state {
	case download.TransferLifecycleProcessed:
		percentDone = 1.0
		leftUntilDone = 0
		status = trStatusSeed
	case download.TransferLifecycleCompleted:
		status = mapPutioStatusValue(in.PutioStatus)
	default:
		status = trStatusDownload
	}

	result := progressResult{
		PercentDone:   percentDone,
		Status:        status,
		LeftUntilDone: leftUntilDone,
	}

	if !localETA.IsZero() {
		result.LocalETA = localETA
		result.LocalSpeed = localSpeed
	}

	return result
}

// mapPutioStatusValue maps a Put.io transfer status string to a Transmission status code.
func mapPutioStatusValue(status string) int {
	switch status {
	case "IN_QUEUE":
		return trStatusDownloadWaiting
	case "DOWNLOADING", "COMPLETING":
		return trStatusDownload
	case "SEEDING", "COMPLETED":
		return trStatusSeed
	case "ERROR":
		return trStatusStopped
	default:
		return trStatusStopped
	}
}
