package download

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/log"
)

const seedingSuffix = ".seeding.json"

// A seeding marker records that the local copy of a transfer is complete while
// the source file stays on Put.io for seeding. The manifest cannot prove this,
// because plundrio writes it before the download starts. The local files cannot
// prove it either, because the *arr app can move them after the import. The
// marker is disk-only and the cleanup hook writes it.
func (m *Manager) seedingPath(id int64) string {
	return filepath.Join(m.transferFiles.stateDir, strconv.FormatInt(id, 10)+seedingSuffix)
}

// SeedingKept reports whether the local copy of the transfer is complete and
// its source file is still on Put.io. If it cannot read the marker for a
// reason other than "not found", it reports "kept", like RemovalPending. A
// wrong "kept" leaves the transfer alone. A wrong "not kept" downloads the
// transfer again.
func (m *Manager) SeedingKept(id int64) bool {
	_, err := os.Lstat(m.seedingPath(id))
	return !os.IsNotExist(err) && !errors.Is(err, syscall.ENOTDIR)
}

// markSeedingKept writes the marker through a temporary file and a rename. It
// syncs the file and the state directory, so the marker stays after a crash.
func (m *Manager) markSeedingKept(id int64) error {
	if err := os.MkdirAll(m.transferFiles.stateDir, 0700); err != nil {
		return fmt.Errorf("create seeding marker directory: %w", err)
	}
	file, err := os.CreateTemp(m.transferFiles.stateDir, ".seeding-*")
	if err != nil {
		return fmt.Errorf("create seeding marker: %w", err)
	}
	defer os.Remove(file.Name())
	if _, err := file.WriteString("{}"); err != nil {
		file.Close()
		return fmt.Errorf("write seeding marker: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync seeding marker: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close seeding marker: %w", err)
	}
	if err := os.Rename(file.Name(), m.seedingPath(id)); err != nil {
		return fmt.Errorf("publish seeding marker: %w", err)
	}
	dir, err := os.Open(m.transferFiles.stateDir)
	if err != nil {
		return fmt.Errorf("open seeding marker directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync seeding marker directory: %w", err)
	}
	return nil
}

// retrySeedingMarkers writes the seeding marker for each processed transfer
// that still has its source file but no marker. This happens when the cleanup
// hook failed to write the marker. Without the marker, a restart downloads the
// transfer again. The monitor calls this function on every poll until the
// write succeeds.
func (m *Manager) retrySeedingMarkers(transfers []*putio.Transfer) {
	if !m.cfg.KeepSeeding {
		return
	}
	// The read lock orders this write with RemoveTransfer. RemoveTransfer
	// drops the context before it removes the marker, so a write cannot
	// bring back the marker of a removed transfer.
	m.removalMu.RLock()
	defer m.removalMu.RUnlock()
	for _, transfer := range transfers {
		ctx, ok := m.coordinator.GetTransferContext(transfer.ID)
		if !ok || ctx.GetState() != TransferLifecycleProcessed || ctx.FileID == 0 {
			continue
		}
		if m.RemovalPending(transfer.ID) || m.SeedingKept(transfer.ID) {
			continue
		}
		if err := m.markSeedingKept(transfer.ID); err != nil {
			log.Error("cleanup").
				Int64("transfer_id", transfer.ID).
				Err(err).
				Msg("Failed again to record that the source file is kept on Put.io")
			continue
		}
		log.Info("cleanup").
			Int64("transfer_id", transfer.ID).
			Msg("Recorded that the source file is kept on Put.io")
	}
}

func (m *Manager) removeSeedingMarker(id int64) error {
	err := os.Remove(m.seedingPath(id))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove seeding marker: %w", err)
	}
	return nil
}
