package download

import (
	"sync"
	"time"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/api"
	"github.com/elsbrock/plundrio/internal/log"
)

// TransferCacheImpl implements the TransferCache interface
type TransferCacheImpl struct {
	transfers       map[string]*PutioTransfer // Map of hash -> transfer
	hashToID        map[string]int64          // Map of hash -> transfer ID
	idToHash        map[int64]string          // Map of transfer ID -> hash
	lastUpdated     time.Time                 // When the cache was last updated
	updateLock      sync.RWMutex              // Lock for cache updates
	client          *api.Client               // API client for fetching transfers
	config          *DownloadConfig           // Configuration
	progressTracker *ProgressTrackerImpl      // Progress tracker
}

// NewTransferCache creates a new transfer cache
func NewTransferCache(client *api.Client, config *DownloadConfig, progressTracker *ProgressTrackerImpl) *TransferCacheImpl {
	return &TransferCacheImpl{
		transfers:       make(map[string]*PutioTransfer),
		hashToID:        make(map[string]int64),
		idToHash:        make(map[int64]string),
		client:          client,
		config:          config,
		progressTracker: progressTracker,
	}
}

// GetTransferByHash returns a cached transfer by its hash
func (tc *TransferCacheImpl) GetTransferByHash(hash string) (*PutioTransfer, bool) {
	tc.updateLock.RLock()
	defer tc.updateLock.RUnlock()

	transfer, ok := tc.transfers[hash]
	return transfer, ok
}

// GetTransferByID returns a cached transfer by its ID
func (tc *TransferCacheImpl) GetTransferByID(id int64) (*PutioTransfer, bool) {
	tc.updateLock.RLock()
	defer tc.updateLock.RUnlock()

	hash, ok := tc.idToHash[id]
	if !ok {
		return nil, false
	}

	transfer, ok := tc.transfers[hash]
	return transfer, ok
}

// GetAllTransfers returns all cached transfers
func (tc *TransferCacheImpl) GetAllTransfers() []*PutioTransfer {
	tc.updateLock.RLock()
	defer tc.updateLock.RUnlock()

	transfers := make([]*PutioTransfer, 0, len(tc.transfers))
	for _, transfer := range tc.transfers {
		transfers = append(transfers, transfer)
	}
	return transfers
}

// UpdateCache refreshes the transfer cache from the API
func (tc *TransferCacheImpl) UpdateCache() error {
	// Check if we need to update based on the configured interval
	if time.Since(tc.lastUpdated) < tc.config.CacheUpdateInterval {
		return nil
	}

	tc.updateLock.Lock()
	defer tc.updateLock.Unlock()

	// Fetch transfers from the API
	transfers, err := tc.client.GetTransfers()
	if err != nil {
		log.Error("cache").Err(err).Msg("Failed to update transfer cache")
		return err
	}

	// Clear progress tracker cache to ensure fresh calculations
	tc.progressTracker.ClearCache()

	// Update the cache
	newTransfers := make(map[string]*PutioTransfer)
	newHashToID := make(map[string]int64)
	newIDToHash := make(map[int64]string)

	for _, t := range transfers {
		if t.Hash == "" {
			continue // Skip transfers without a hash
		}

		// Create cached transfer
		cachedTransfer := &PutioTransfer{
			ID:             t.ID,
			Hash:           t.Hash,
			Name:           t.Name,
			Status:         t.Status,
			Size:           int64(t.Size),
			Downloaded:     t.Downloaded,
			FileID:         t.FileID,
			SecondsSeeding: t.SecondsSeeding,
			ErrorMessage:   t.ErrorMessage,
			Availability:   t.Availability,
			PercentDone:    t.PercentDone,
		}

		if t.CreatedAt != nil {
			cachedTransfer.CreatedAt = t.CreatedAt.Time
		}

		if t.FinishedAt != nil {
			cachedTransfer.FinishedAt = t.FinishedAt.Time
		}

		// Store in maps
		newTransfers[t.Hash] = cachedTransfer
		newHashToID[t.Hash] = t.ID
		newIDToHash[t.ID] = t.Hash
	}

	// Replace the maps atomically
	tc.transfers = newTransfers
	tc.hashToID = newHashToID
	tc.idToHash = newIDToHash
	tc.lastUpdated = time.Now()

	log.Info("cache").
		Int("transfers", len(tc.transfers)).
		Msg("Transfer cache updated")

	return nil
}

// UpdateTransferProgress updates the local download progress for a transfer
func (tc *TransferCacheImpl) UpdateTransferProgress(transferID int64, downloadedSize int64) {
	tc.updateLock.Lock()
	defer tc.updateLock.Unlock()

	hash, ok := tc.idToHash[transferID]
	if !ok {
		return
	}

	transfer, ok := tc.transfers[hash]
	if !ok {
		return
	}

	transfer.DownloadedSize = downloadedSize

	// Invalidate progress cache for this transfer
	tc.progressTracker.InvalidateCache(transferID)
}

// AddTransfer adds a new transfer to the cache
func (tc *TransferCacheImpl) AddTransfer(transfer *putio.Transfer) {
	if transfer.Hash == "" {
		return // Skip transfers without a hash
	}

	tc.updateLock.Lock()
	defer tc.updateLock.Unlock()

	// Create cached transfer
	cachedTransfer := &PutioTransfer{
		ID:             transfer.ID,
		Hash:           transfer.Hash,
		Name:           transfer.Name,
		Status:         transfer.Status,
		Size:           int64(transfer.Size),
		Downloaded:     transfer.Downloaded,
		FileID:         transfer.FileID,
		SecondsSeeding: transfer.SecondsSeeding,
		ErrorMessage:   transfer.ErrorMessage,
		Availability:   transfer.Availability,
		PercentDone:    transfer.PercentDone,
	}

	if transfer.CreatedAt != nil {
		cachedTransfer.CreatedAt = transfer.CreatedAt.Time
	}

	if transfer.FinishedAt != nil {
		cachedTransfer.FinishedAt = transfer.FinishedAt.Time
	}

	// Store in maps
	tc.transfers[transfer.Hash] = cachedTransfer
	tc.hashToID[transfer.Hash] = transfer.ID
	tc.idToHash[transfer.ID] = transfer.Hash

	log.Debug("cache").
		Str("hash", transfer.Hash).
		Int64("id", transfer.ID).
		Msg("Added transfer to cache")
}

// RemoveTransfer removes a transfer from the cache
func (tc *TransferCacheImpl) RemoveTransfer(hash string) {
	tc.updateLock.Lock()
	defer tc.updateLock.Unlock()

	id, ok := tc.hashToID[hash]
	if !ok {
		return
	}

	delete(tc.transfers, hash)
	delete(tc.hashToID, hash)
	delete(tc.idToHash, id)

	// Invalidate progress cache for this transfer
	tc.progressTracker.InvalidateCache(id)

	log.Debug("cache").
		Str("hash", hash).
		Int64("id", id).
		Msg("Removed transfer from cache")
}
