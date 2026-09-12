package download

import (
	"testing"

	"github.com/elsbrock/go-putio"
)

// markFailed drives a transfer into the Failed lifecycle state the way a real
// permanent download failure would.
func markFailed(t *testing.T, m *Manager, transferID int64) {
	t.Helper()
	m.coordinator.InitiateTransfer(transferID, "stuck", 100, 1)
	if err := m.coordinator.StartDownload(transferID); err != nil {
		t.Fatalf("StartDownload: %v", err)
	}
	if err := m.coordinator.FileFailure(transferID); err != nil {
		t.Fatalf("FileFailure: %v", err)
	}
	ctx, ok := m.coordinator.GetTransferContext(transferID)
	if !ok || ctx.GetState() != TransferLifecycleFailed {
		t.Fatalf("expected transfer to be in Failed state")
	}
}

// Regression test: a transfer whose local download failed used to keep its
// coordinator context forever, and the "already being processed" check treated
// any tracked transfer as in flight. The transfer was therefore never retried.
func TestFailedTransferIsReprocessed(t *testing.T) {
	m := newManagerForTest(t, &fakeClient{})
	transfer := &putio.Transfer{ID: 1, Name: "stuck", Status: "COMPLETED", SaveParentID: testFolderID}

	markFailed(t, m, transfer.ID)

	if !m.processor.shouldProcess(transfer) {
		t.Fatal("failed transfer should be retried, but was skipped")
	}
	// The stale context must be gone so the retry starts from scratch.
	if _, ok := m.coordinator.GetTransferContext(transfer.ID); ok {
		t.Error("expected the failed transfer's context to be dropped before retry")
	}
}

// Retries must be bounded, or a permanently broken file would be re-attempted
// on every 30s poll forever.
func TestFailedTransferStopsAfterMaxAttempts(t *testing.T) {
	m := newManagerForTest(t, &fakeClient{})
	transfer := &putio.Transfer{ID: 1, Name: "stuck", Status: "COMPLETED", SaveParentID: testFolderID}

	for attempt := 1; attempt <= maxReprocessAttempts; attempt++ {
		markFailed(t, m, transfer.ID)
		if !m.processor.shouldProcess(transfer) {
			t.Fatalf("attempt %d should have been allowed", attempt)
		}
	}

	markFailed(t, m, transfer.ID)
	if m.processor.shouldProcess(transfer) {
		t.Fatalf("transfer should be left alone after %d attempts", maxReprocessAttempts)
	}
}

// A transfer that is mid-flight or already done must never be restarted.
func TestShouldProcessSkipsNonFailedStates(t *testing.T) {
	transfer := &putio.Transfer{ID: 1, Name: "t", Status: "COMPLETED", SaveParentID: testFolderID}

	for _, tc := range []struct {
		name  string
		drive func(m *Manager)
	}{
		{"downloading", func(m *Manager) {
			m.coordinator.InitiateTransfer(1, "t", 100, 2)
			_ = m.coordinator.StartDownload(1)
		}},
		{"completed", func(m *Manager) {
			m.coordinator.InitiateTransfer(1, "t", 100, 1)
			_ = m.coordinator.StartDownload(1)
			_ = m.coordinator.FileCompleted(1)
		}},
		{"processed", func(m *Manager) {
			m.coordinator.InitiateTransfer(1, "t", 100, 1)
			_ = m.coordinator.StartDownload(1)
			_ = m.coordinator.FileCompleted(1)
			_ = m.coordinator.CompleteTransfer(1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newManagerForTest(t, &fakeClient{})
			tc.drive(m)
			if m.processor.shouldProcess(transfer) {
				t.Errorf("transfer in %s state should not be reprocessed", tc.name)
			}
		})
	}
}

// An unseen transfer is always processed.
func TestShouldProcessUntrackedTransfer(t *testing.T) {
	m := newManagerForTest(t, &fakeClient{})
	transfer := &putio.Transfer{ID: 7, Name: "new", Status: "COMPLETED", SaveParentID: testFolderID}
	if !m.processor.shouldProcess(transfer) {
		t.Fatal("an untracked transfer should be processed")
	}
}

// Retrying while sibling files are still downloading would queue a second
// generation of the same files, so a failed transfer must settle first.
func TestFailedTransferWaitsForInFlightFiles(t *testing.T) {
	m := newManagerForTest(t, &fakeClient{})
	transfer := &putio.Transfer{ID: 1, Name: "stuck", Status: "COMPLETED", SaveParentID: testFolderID}

	markFailed(t, m, transfer.ID)
	m.activeFiles.Store(int64(555), transfer.ID) // a sibling file still downloading

	if m.processor.shouldProcess(transfer) {
		t.Fatal("should not retry while files are still in flight")
	}

	m.activeFiles.Delete(int64(555))
	if !m.processor.shouldProcess(transfer) {
		t.Fatal("should retry once the transfer has settled")
	}
}

// A transfer that reports no files must end up tracked as Failed. Previously
// FailTransfer ran before the context existed, so it silently did nothing and
// the transfer was re-examined on every poll forever.
func TestTransferWithNoFilesIsMarkedFailed(t *testing.T) {
	m := newManagerForTest(t, &fakeClient{
		files: func(int64) ([]*putio.File, error) { return nil, nil },
	})
	transfer := &putio.Transfer{ID: 1, Name: "empty", Status: "COMPLETED", SaveParentID: testFolderID, FileID: 100}

	m.processor.processTransfer(transfer)

	ctx, ok := m.coordinator.GetTransferContext(transfer.ID)
	if !ok {
		t.Fatal("expected the empty transfer to be tracked")
	}
	if state := ctx.GetState(); state != TransferLifecycleFailed {
		t.Errorf("expected Failed state, got %s", state)
	}
	if err := ctx.GetError(); err == nil {
		t.Error("expected the failure reason to be recorded")
	}
}

// RemoveTransfer must drop every piece of local bookkeeping, otherwise the
// maps grow for the lifetime of the process.
func TestRemoveTransferClearsAllBookkeeping(t *testing.T) {
	m := newManagerForTest(t, &fakeClient{})
	transfer := &putio.Transfer{ID: 1, Name: "stuck", Status: "COMPLETED", SaveParentID: testFolderID}

	markFailed(t, m, transfer.ID)
	m.processor.shouldProcess(transfer) // records a reprocess attempt
	m.processor.retryAttempts.Store(transfer.ID, 2)
	if err := m.transferFiles.Set(transfer.ID, []TransferFile{{Name: "stuck/file.m4b", Length: 1}}); err != nil {
		t.Fatal(err)
	}

	m.RemoveTransfer(transfer.ID)

	if _, ok := m.coordinator.GetTransferContext(transfer.ID); ok {
		t.Error("coordinator context should be gone")
	}
	if _, ok := m.processor.reprocessAttempts.Load(transfer.ID); ok {
		t.Error("reprocess attempts should be cleared")
	}
	if _, ok := m.processor.retryAttempts.Load(transfer.ID); ok {
		t.Error("retry attempts should be cleared")
	}
	if _, ok := m.transferFiles.Get(transfer.ID); ok {
		t.Error("transfer file manifest should be gone")
	}
}
