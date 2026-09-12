package download

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/api"
	"github.com/elsbrock/plundrio/internal/log"
)

const reviewSuffix = ".review.json"

// NeedsReviewMessage describes uncertainty, not a failed local download.
const NeedsReviewMessage = "Local completion is unverified: source unavailable and no file manifest exists. Operator review required."

type transferReview struct {
	ID           int64  `json:"id"`
	FileID       int64  `json:"file_id"`
	Name         string `json:"name"`
	SaveParentID int64  `json:"save_parent_id"`
	Category     string `json:"category"`
}

func (m *Manager) reviewPath(id int64) string {
	return filepath.Join(m.transferFiles.stateDir, strconv.FormatInt(id, 10)+reviewSuffix)
}

// NeedsReview fails closed on unreadable or malformed state. It remains true
// during pending retirement, after the in-memory context has been forgotten.
func (m *Manager) NeedsReview(id int64) bool {
	_, err := os.Lstat(m.reviewPath(id))
	return !os.IsNotExist(err) && !errors.Is(err, syscall.ENOTDIR)
}

func (m *Manager) loadReview(id int64) (transferReview, error) {
	var review transferReview
	info, err := os.Lstat(m.reviewPath(id))
	if err != nil {
		return review, err
	}
	if !info.Mode().IsRegular() {
		return review, fmt.Errorf("review marker must be a regular file")
	}
	data, err := os.ReadFile(m.reviewPath(id))
	if err != nil {
		return review, err
	}
	if err := json.Unmarshal(data, &review); err != nil {
		return review, fmt.Errorf("parse review marker: %w", err)
	}
	if id <= 0 || review.ID != id || review.FileID < 0 || review.Name == "" {
		return review, fmt.Errorf("invalid review transfer identity")
	}
	if review.Category != "" && (!filepath.IsLocal(review.Category) || filepath.Clean(review.Category) != review.Category) {
		return review, fmt.Errorf("invalid review category %q", review.Category)
	}
	return review, nil
}

func (m *Manager) publishReview(transfer *putio.Transfer) {
	if current, ok := m.coordinator.GetTransferContext(transfer.ID); ok && current.GetState() == TransferLifecycleNeedsReview {
		return
	}
	ctx := m.coordinator.InitiateTransfer(transfer.ID, transfer.Name, transfer.FileID, 0)
	ctx.mu.Lock()
	ctx.state = TransferLifecycleNeedsReview
	ctx.err = nil
	ctx.mu.Unlock()
}

// markNeedsReview is called with removalMu held for reading, like manifest
// publication. It never starts work or calls a cleanup hook, even if storage
// fails; the caller must preserve the review state when reporting that error.
func (m *Manager) markNeedsReview(transfer *putio.Transfer) error {
	if transfer == nil || transfer.ID <= 0 || transfer.FileID < 0 || transfer.Name == "" {
		return fmt.Errorf("review requires a valid transfer identity")
	}
	if m.RemovalPending(transfer.ID) {
		return fmt.Errorf("transfer %d removal is pending", transfer.ID)
	}
	m.publishReview(transfer)
	current, _ := m.GetTransferContext(transfer.ID)
	current.mu.Lock()
	if current.review == nil {
		current.review = &transferReview{ID: transfer.ID, FileID: transfer.FileID, Name: transfer.Name,
			SaveParentID: transfer.SaveParentID, Category: m.categories.Get(transfer.ID)}
	}
	review := *current.review
	current.mu.Unlock()
	if m.NeedsReview(transfer.ID) {
		_, err := m.loadReview(transfer.ID)
		return err
	}
	if err := m.requireAbsentReviewManifest(transfer.ID); err != nil {
		return err
	}
	data, err := json.Marshal(review)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.transferFiles.stateDir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(m.transferFiles.stateDir, ".review-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), m.reviewPath(transfer.ID)); err != nil {
		return err
	}
	dir, err := os.Open(m.transferFiles.stateDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// restoreReview must run before grouping transfers by remote status. A remote
// ERROR or reappearing source cannot override the durable operator hold.
// Call outside removalMu: this method takes its own read lock.
func (m *Manager) restoreReview(transfer *putio.Transfer) bool {
	m.removalMu.RLock()
	defer m.removalMu.RUnlock()
	if !m.NeedsReview(transfer.ID) {
		current, ok := m.GetTransferContext(transfer.ID)
		if !ok || current.GetState() != TransferLifecycleNeedsReview {
			return false
		}
		// A previous persistence attempt may have failed. Repairing storage
		// must make the existing hold durable, not require releasing it first.
		original := *transfer
		original.Name, original.FileID = current.Name, current.FileID
		if err := m.markNeedsReview(&original); err != nil {
			log.Error("transfers").Int64("transfer_id", transfer.ID).Err(err).
				Msg("Review hold remains in memory; durable persistence still unavailable")
		}
		return true
	}
	if m.RemovalPending(transfer.ID) {
		return true
	}
	review, err := m.loadReview(transfer.ID)
	if err != nil {
		log.Error("transfers").Int64("transfer_id", transfer.ID).Err(err).
			Msg("Invalid review marker; operator hold retained")
	} else {
		original := *transfer
		original.Name, original.FileID = review.Name, review.FileID
		transfer = &original
	}
	m.publishReview(transfer)
	return true
}

func (m *Manager) requireAbsentReviewManifest(id int64) error {
	_, err := os.Lstat(m.transferFiles.path(id))
	if err == nil {
		return fmt.Errorf("transfer %d has an existing manifest; review retirement requires absent state", id)
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("review retirement requires an absent manifest: %w", err)
	}
	return nil
}

// PrepareReviewRetirement prepares exact record-only removal. The RPC caller
// must validate explicit retirement and retained-copy acknowledgments first,
// and must call only DeleteTransfer after this succeeds, never DeleteFile or
// local data deletion. The durable review marker protects retries and restarts.
func (m *Manager) PrepareReviewRetirement(ctx context.Context, transfer *putio.Transfer) (string, error) {
	if transfer == nil || transfer.ID <= 0 {
		return "", fmt.Errorf("review retirement requires a positive transfer ID")
	}
	review, err := m.loadReview(transfer.ID)
	if err != nil {
		return "", fmt.Errorf("read review state: %w", err)
	}
	if err := m.requireAbsentReviewManifest(transfer.ID); err != nil {
		return "", err
	}
	// Refresh identity without holding the queue/removal lock across the API.
	transfers, err := m.client.GetTransfers(ctx)
	if err != nil {
		return "", fmt.Errorf("refresh reviewed transfer: %w", err)
	}
	var current *putio.Transfer
	for _, candidate := range transfers {
		if candidate.ID == transfer.ID {
			current = candidate
			break
		}
	}
	if current == nil || current.Name != review.Name || current.FileID != review.FileID || current.SaveParentID != review.SaveParentID {
		return "", fmt.Errorf("reviewed transfer identity is absent or changed")
	}
	if current.Status != "COMPLETED" && current.Status != "SEEDING" {
		return "", fmt.Errorf("reviewed transfer must remain remotely completed")
	}
	if current.FileID != 0 {
		_, err := m.client.GetAllTransferFiles(ctx, current.FileID)
		var missing *api.TransferSourceNotFoundError
		if !errors.As(err, &missing) || missing.FileID != current.FileID {
			return "", fmt.Errorf("reviewed source absence could not be confirmed")
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	m.removalMu.Lock()
	defer m.removalMu.Unlock()
	latest, err := m.loadReview(transfer.ID)
	if err != nil || latest != review {
		return "", fmt.Errorf("review state changed during retirement preparation")
	}
	if err := m.requireAbsentReviewManifest(transfer.ID); err != nil {
		return "", err
	}
	if m.activeFileCount(transfer.ID) != 0 {
		return "", fmt.Errorf("reviewed transfer still has active downloads")
	}
	return m.prepareRemovalLocked(transfer.ID, review.Category)
}

func (m *Manager) removeReview(id int64) error {
	err := os.Remove(m.reviewPath(id))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove review marker: %w", err)
	}
	return nil
}
