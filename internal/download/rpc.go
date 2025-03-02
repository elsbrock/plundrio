package download

import (
	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/log"
)

// RPCHandler provides methods for handling RPC requests related to transfers
type RPCHandler struct {
	processor       *TransferProcessor
	transferCache   *TransferCacheImpl
	progressTracker *ProgressTrackerImpl
}

// NewRPCHandler creates a new RPC handler
func NewRPCHandler(processor *TransferProcessor) *RPCHandler {
	return &RPCHandler{
		processor:       processor,
		transferCache:   processor.transferCache,
		progressTracker: processor.progressTracker,
	}
}

// GetTransferByHash returns a transfer by its hash
func (h *RPCHandler) GetTransferByHash(hash string) (*PutioTransfer, bool) {
	// Update the cache if needed
	if err := h.transferCache.UpdateCache(); err != nil {
		log.Error("rpc").Err(err).Msg("Failed to update transfer cache")
	}

	// Get the transfer from the cache
	return h.transferCache.GetTransferByHash(hash)
}

// GetAllTransfers returns all transfers from the cache
// This is used by the server/torrent.go handleTorrentGet method
func (h *RPCHandler) GetAllTransfers() []*PutioTransfer {
	// Update the cache if needed
	if err := h.transferCache.UpdateCache(); err != nil {
		log.Error("rpc").Err(err).Msg("Failed to update transfer cache")
	}

	// Get all transfers from the cache
	return h.transferCache.GetAllTransfers()
}

// GetProgress returns the progress for a transfer
func (h *RPCHandler) GetProgress(transferID int64, putioTransfer *putio.Transfer) (float64, int64) {
	return h.progressTracker.GetProgress(transferID, putioTransfer)
}

// AddTransfer adds a new transfer to the cache
func (h *RPCHandler) AddTransfer(transfer *putio.Transfer) {
	h.transferCache.AddTransfer(transfer)
}

// RemoveTransfer removes a transfer from the cache
func (h *RPCHandler) RemoveTransfer(hash string) {
	h.transferCache.RemoveTransfer(hash)
}

// GetTransferIDByHash returns the transfer ID for a given hash
func (h *RPCHandler) GetTransferIDByHash(hash string) (int64, bool) {
	h.transferCache.updateLock.RLock()
	defer h.transferCache.updateLock.RUnlock()

	id, ok := h.transferCache.hashToID[hash]
	return id, ok
}

// IsTransferProcessed checks if a transfer has been processed locally
func (h *RPCHandler) IsTransferProcessed(transferID int64) bool {
	_, processed := h.processor.processedTransfers.Load(transferID)
	return processed
}

// GetTransferContext returns the transfer context for a given transfer ID
func (h *RPCHandler) GetTransferContext(transferID int64) (*TransferContext, bool) {
	return h.processor.manager.coordinator.GetTransferContext(transferID)
}
