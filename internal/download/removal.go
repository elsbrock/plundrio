package download

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/log"
)

const removalSuffix = ".removing.json"

// Removal markers are disk-only. Keeping them separate from manifests preserves
// the existing file format and exact ownership while releasing process memory.
func (m *Manager) removalPath(id int64) string {
	return filepath.Join(m.transferFiles.stateDir, strconv.FormatInt(id, 10)+removalSuffix)
}

// RemovalPending fails closed on filesystem errors, including unreadable markers.
func (m *Manager) RemovalPending(id int64) bool {
	_, err := os.Lstat(m.removalPath(id))
	// ENOTDIR also proves no marker exists; manifest creation will report the
	// broken state directory through the ordinary bounded Failed lifecycle.
	return !os.IsNotExist(err) && !errors.Is(err, syscall.ENOTDIR)
}

func (m *Manager) removalCategory(id int64) (string, error) {
	data, err := os.ReadFile(m.removalPath(id))
	if err != nil {
		return "", err
	}
	var storedCategory *string
	if err := json.Unmarshal(data, &storedCategory); err != nil {
		return "", fmt.Errorf("read removal category: %w", err)
	}
	if storedCategory == nil {
		return "", fmt.Errorf("removal category must be a JSON string")
	}
	category := *storedCategory
	if category != "" && (!filepath.IsLocal(category) || filepath.Clean(category) != category) {
		return "", fmt.Errorf("invalid removal category %q", category)
	}
	return category, nil
}

// PrepareRemoval durably suppresses new processing before remote deletion.
// The lock serializes context publication and queue admission, never network
// calls or blocked sends. Claimed jobs retain suppression until workers drain.
// The returned category is captured under the same lock as marker reclamation.
func (m *Manager) PrepareRemoval(id int64, requireClassified bool) (string, error) {
	m.removalMu.Lock()
	defer m.removalMu.Unlock()
	if m.NeedsReview(id) {
		return "", fmt.Errorf("transfer %d requires explicit reviewed record-only retirement", id)
	}
	if ctx, ok := m.coordinator.GetTransferContext(id); ok && ctx.GetState() == TransferLifecycleNeedsReview {
		return "", fmt.Errorf("transfer %d requires explicit reviewed record-only retirement", id)
	}
	// Ready remote records require classification, checked atomically with
	// retry-generation replacement and removal suppression. Pending ordinary
	// removals have already forgotten their context and remain retryable.
	if requireClassified && !m.RemovalPending(id) {
		ctx, ok := m.coordinator.GetTransferContext(id)
		if !ok || ctx.GetState() == TransferLifecycleInitial {
			return "", fmt.Errorf("transfer %d local state is still being classified; retry after the next monitor poll", id)
		}
	}
	return m.prepareRemovalLocked(id, m.categories.Get(id))
}

// prepareRemovalLocked requires removalMu's write lock. Review retirement uses
// the same suppression mechanism while retaining its separate review marker.
func (m *Manager) prepareRemovalLocked(id int64, category string) (string, error) {
	if m.RemovalPending(id) {
		var err error
		category, err = m.removalCategory(id)
		if err != nil {
			return "", err
		}
	} else {
		if id <= 0 {
			return "", fmt.Errorf("removal requires a positive transfer ID")
		}
		if err := os.MkdirAll(m.transferFiles.stateDir, 0700); err != nil {
			return "", err
		}
		data, err := json.Marshal(category)
		if err != nil {
			return "", err
		}
		file, err := os.CreateTemp(m.transferFiles.stateDir, ".removing-*")
		if err != nil {
			return "", err
		}
		defer os.Remove(file.Name())
		if _, err := file.Write(data); err != nil {
			file.Close()
			return "", err
		}
		if err := file.Sync(); err != nil {
			file.Close()
			return "", err
		}
		if err := file.Close(); err != nil {
			return "", err
		}
		if err := os.Rename(file.Name(), m.removalPath(id)); err != nil {
			return "", err
		}
	}
	m.processor.forget(id)
	m.categories.Remove(id)
	log.Warn("transfers").Int64("transfer_id", id).
		Msg("Transfer removal pending; released memory and suspended processing until remote deletion succeeds")
	return category, nil
}

// Snapshot only existing markers BEFORE fetching the full remote list. A marker
// created concurrently with that request cannot be mistaken for an absent ID.
func (m *Manager) pendingRemovals() []int64 {
	entries, err := os.ReadDir(m.transferFiles.stateDir)
	if err != nil {
		return nil
	}
	var ids []int64
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), removalSuffix) {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimSuffix(entry.Name(), removalSuffix), 10, 64)
		if err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

// Only a successful account-wide listing proves remote absence. A move outside
// managed folders must not discard suppression or ownership evidence.
func (m *Manager) pruneRemovals(pending []int64, transfers []*putio.Transfer) {
	present := make(map[int64]bool, len(transfers))
	for _, transfer := range transfers {
		present[transfer.ID] = true
	}
	for _, id := range pending {
		if !present[id] && m.activeFileCount(id) == 0 {
			m.RemoveTransfer(id)
		}
	}
}
