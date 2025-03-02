package download

import (
	"sync"

	"github.com/elsbrock/go-putio"
)

// ProgressTracker handles two-phase progress calculation
type ProgressTrackerImpl struct {
	cache       map[int64]float64 // Cache of transfer ID -> progress
	cacheLock   sync.RWMutex      // Lock for cache access
	processor   *TransferProcessor
	coordinator *TransferCoordinator
	config      *DownloadConfig
}

// NewProgressTracker creates a new progress tracker
func NewProgressTracker(processor *TransferProcessor, coordinator *TransferCoordinator, config *DownloadConfig) *ProgressTrackerImpl {
	return &ProgressTrackerImpl{
		cache:       make(map[int64]float64),
		processor:   processor,
		coordinator: coordinator,
		config:      config,
	}
}

// GetProgress calculates and returns the progress for a transfer
// Progress is calculated in two phases:
// - Put.io progress (0-100%) is mapped to 0-50% of the total progress
// - Local download progress (0-100%) is mapped to 50-100% of the total progress
func (pt *ProgressTrackerImpl) GetProgress(transferID int64, putioTransfer *putio.Transfer) (float64, int64) {
	// Check cache first
	pt.cacheLock.RLock()
	if progress, ok := pt.cache[transferID]; ok {
		pt.cacheLock.RUnlock()
		return progress, 0 // Cached progress, remaining bytes unknown
	}
	pt.cacheLock.RUnlock()

	// For seeding transfers, return 100%
	if putioTransfer.Status == "SEEDING" {
		pt.cacheProgress(transferID, 100.0)
		return 100.0, 0
	}

	// Calculate put.io progress (0-50%)
	putioProgress := float64(putioTransfer.PercentDone) / 2.0

	// Get local progress if available
	localProgress := 0.0
	var remainingBytes int64 = 0

	// Get transfer context to check local progress
	ctx, exists := pt.coordinator.GetTransferContext(transferID)
	if exists {
		ctx.mu.RLock()
		if ctx.TotalSize > 0 {
			localProgress = float64(ctx.DownloadedSize) / float64(ctx.TotalSize) * 50.0
			remainingBytes = ctx.TotalSize - ctx.DownloadedSize
		} else if ctx.TotalFiles > 0 {
			localProgress = float64(ctx.CompletedFiles) / float64(ctx.TotalFiles) * 50.0
			// Can't calculate remaining bytes without size info
		}

		// If transfer is processed, return 100%
		if ctx.Processed == Processed {
			localProgress = 50.0
		}
		ctx.mu.RUnlock()
	}

	// Calculate total progress
	totalProgress := putioProgress + localProgress

	// Ensure progress is between 0 and 100
	if totalProgress < 0 {
		totalProgress = 0
	} else if totalProgress > 100 {
		totalProgress = 100
	}

	// Cache the result
	pt.cacheProgress(transferID, totalProgress)

	return totalProgress, remainingBytes
}

// cacheProgress stores the progress in the cache
func (pt *ProgressTrackerImpl) cacheProgress(transferID int64, progress float64) {
	pt.cacheLock.Lock()
	defer pt.cacheLock.Unlock()
	pt.cache[transferID] = progress
}

// ClearCache clears the progress cache
func (pt *ProgressTrackerImpl) ClearCache() {
	pt.cacheLock.Lock()
	defer pt.cacheLock.Unlock()
	pt.cache = make(map[int64]float64)
}

// InvalidateCache removes a specific transfer from the cache
func (pt *ProgressTrackerImpl) InvalidateCache(transferID int64) {
	pt.cacheLock.Lock()
	defer pt.cacheLock.Unlock()
	delete(pt.cache, transferID)
}
