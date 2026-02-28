package download

import (
	"errors"
	"sync"
	"testing"

	"github.com/elsbrock/plundrio/internal/config"
)

// newTestManager creates a minimal Manager with just enough fields
// for the TransferCoordinator to work. The coordinator's onProcessed
// callback calls processor.MarkTransferProcessed(), so we wire up
// a real TransferProcessor with a minimal config.
func newTestManager() *Manager {
	cfg := &config.Config{
		TargetDir:   "/tmp/plundrio-test",
		WorkerCount: 1,
	}
	dlConfig := GetDefaultConfig()

	m := &Manager{
		cfg:        cfg,
		dlConfig:   dlConfig,
		categories: newCategoryStore(cfg.TargetDir),
		stopChan:   make(chan struct{}),
		jobs:       make(chan downloadJob, 5),
	}
	m.processor = newTransferProcessor(m)
	m.coordinator = NewTransferCoordinator(func(transferID int64) {
		m.processor.MarkTransferProcessed(transferID)
	})
	return m
}

func TestCoordinatorHappyPath(t *testing.T) {
	m := newTestManager()
	tc := m.coordinator

	// Initiate a transfer with 3 files
	ctx := tc.InitiateTransfer(1, "test-transfer", 100, 3)
	if ctx.GetState() != TransferLifecycleInitial {
		t.Fatalf("expected Initial state, got %s", ctx.GetState())
	}

	// Start download
	if err := tc.StartDownload(1); err != nil {
		t.Fatalf("StartDownload failed: %v", err)
	}
	if ctx.GetState() != TransferLifecycleDownloading {
		t.Fatalf("expected Downloading state, got %s", ctx.GetState())
	}

	// Complete files one by one
	for i := 0; i < 3; i++ {
		if err := tc.FileCompleted(1); err != nil {
			t.Fatalf("FileCompleted(%d) failed: %v", i, err)
		}
	}

	// After all files complete with no failures, state should be Completed
	if ctx.GetState() != TransferLifecycleCompleted {
		t.Fatalf("expected Completed state after all files done, got %s", ctx.GetState())
	}

	// CompleteTransfer should transition to Processed
	if err := tc.CompleteTransfer(1); err != nil {
		t.Fatalf("CompleteTransfer failed: %v", err)
	}
	if ctx.GetState() != TransferLifecycleProcessed {
		t.Fatalf("expected Processed state, got %s", ctx.GetState())
	}

	// Verify the processor recorded it
	if _, ok := m.processor.processedTransfers.Load(int64(1)); !ok {
		t.Fatal("expected transfer to be marked as processed in processor")
	}
}

func TestCoordinatorStartDownloadFromNonInitialState(t *testing.T) {
	m := newTestManager()
	tc := m.coordinator

	tc.InitiateTransfer(1, "test", 100, 1)

	// Move to Downloading first
	if err := tc.StartDownload(1); err != nil {
		t.Fatalf("first StartDownload failed: %v", err)
	}

	// Attempting StartDownload again should fail
	err := tc.StartDownload(1)
	if err == nil {
		t.Fatal("expected error when calling StartDownload from Downloading state")
	}
}

func TestCoordinatorFileCompletedIdempotent(t *testing.T) {
	m := newTestManager()
	tc := m.coordinator

	ctx := tc.InitiateTransfer(1, "test", 100, 1)

	if err := tc.StartDownload(1); err != nil {
		t.Fatalf("StartDownload failed: %v", err)
	}

	// Complete the single file -> state becomes Completed
	if err := tc.FileCompleted(1); err != nil {
		t.Fatalf("FileCompleted failed: %v", err)
	}
	if ctx.GetState() != TransferLifecycleCompleted {
		t.Fatalf("expected Completed, got %s", ctx.GetState())
	}

	// Calling FileCompleted again on a Completed transfer should return nil
	if err := tc.FileCompleted(1); err != nil {
		t.Fatalf("expected nil on idempotent FileCompleted, got: %v", err)
	}

	// CompletedFiles should still be 1 (not incremented again)
	_, _, completedFiles, _ := ctx.GetProgress()
	if completedFiles != 1 {
		t.Fatalf("expected CompletedFiles=1, got %d", completedFiles)
	}
}

func TestCoordinatorFileCompletedNotFound(t *testing.T) {
	m := newTestManager()
	tc := m.coordinator

	// FileCompleted on nonexistent transfer returns nil (graceful)
	if err := tc.FileCompleted(999); err != nil {
		t.Fatalf("expected nil for unknown transfer, got: %v", err)
	}
}

func TestCoordinatorFileFailureMarksStateFailed(t *testing.T) {
	m := newTestManager()
	tc := m.coordinator

	ctx := tc.InitiateTransfer(1, "test", 100, 2)

	if err := tc.StartDownload(1); err != nil {
		t.Fatalf("StartDownload failed: %v", err)
	}

	// Fail one file
	if err := tc.FileFailure(1); err != nil {
		t.Fatalf("FileFailure failed: %v", err)
	}

	if ctx.GetState() != TransferLifecycleFailed {
		t.Fatalf("expected Failed state, got %s", ctx.GetState())
	}
	_, _, _, failedFiles := ctx.GetProgress()
	if failedFiles != 1 {
		t.Fatalf("expected FailedFiles=1, got %d", failedFiles)
	}
}

func TestCoordinatorMixedCompletionsAndFailures(t *testing.T) {
	m := newTestManager()
	tc := m.coordinator

	// 3 files total: 2 succeed, 1 fails
	ctx := tc.InitiateTransfer(1, "test", 100, 3)

	if err := tc.StartDownload(1); err != nil {
		t.Fatalf("StartDownload failed: %v", err)
	}

	// Complete 2 files
	if err := tc.FileCompleted(1); err != nil {
		t.Fatalf("FileCompleted failed: %v", err)
	}
	if err := tc.FileCompleted(1); err != nil {
		t.Fatalf("FileCompleted failed: %v", err)
	}

	// Fail 1 file - this puts state to Failed, and completed+failed=3=total
	if err := tc.FileFailure(1); err != nil {
		t.Fatalf("FileFailure failed: %v", err)
	}

	// With failed files, state should be Failed even though all files are processed
	if ctx.GetState() != TransferLifecycleFailed {
		t.Fatalf("expected Failed state with mixed results, got %s", ctx.GetState())
	}
	_, _, completedFiles, failedFiles := ctx.GetProgress()
	if completedFiles != 2 {
		t.Fatalf("expected CompletedFiles=2, got %d", completedFiles)
	}
	if failedFiles != 1 {
		t.Fatalf("expected FailedFiles=1, got %d", failedFiles)
	}
}

func TestCoordinatorCompleteTransferWithPendingFiles(t *testing.T) {
	m := newTestManager()
	tc := m.coordinator

	tc.InitiateTransfer(1, "test", 100, 3)

	if err := tc.StartDownload(1); err != nil {
		t.Fatalf("StartDownload failed: %v", err)
	}

	// Only complete 1 of 3 files
	if err := tc.FileCompleted(1); err != nil {
		t.Fatalf("FileCompleted failed: %v", err)
	}

	// CompleteTransfer should fail because 2 files are still pending
	err := tc.CompleteTransfer(1)
	if err == nil {
		t.Fatal("expected error when completing transfer with pending files")
	}
}

func TestCoordinatorCompleteTransferRunsCleanupHooks(t *testing.T) {
	m := newTestManager()
	tc := m.coordinator

	var hookCalls []int64
	var hookMu sync.Mutex

	tc.RegisterCleanupHook(func(transferID int64) error {
		hookMu.Lock()
		defer hookMu.Unlock()
		hookCalls = append(hookCalls, transferID)
		return nil
	})

	tc.RegisterCleanupHook(func(transferID int64) error {
		hookMu.Lock()
		defer hookMu.Unlock()
		hookCalls = append(hookCalls, transferID)
		return nil
	})

	tc.InitiateTransfer(1, "test", 100, 1)
	if err := tc.StartDownload(1); err != nil {
		t.Fatalf("StartDownload failed: %v", err)
	}
	if err := tc.FileCompleted(1); err != nil {
		t.Fatalf("FileCompleted failed: %v", err)
	}

	if err := tc.CompleteTransfer(1); err != nil {
		t.Fatalf("CompleteTransfer failed: %v", err)
	}

	hookMu.Lock()
	defer hookMu.Unlock()
	if len(hookCalls) != 2 {
		t.Fatalf("expected 2 cleanup hook calls, got %d", len(hookCalls))
	}
	for _, id := range hookCalls {
		if id != 1 {
			t.Fatalf("cleanup hook called with wrong ID: %d", id)
		}
	}
}

func TestCoordinatorCleanupHookErrorDoesNotBlockCompletion(t *testing.T) {
	m := newTestManager()
	tc := m.coordinator

	tc.RegisterCleanupHook(func(transferID int64) error {
		return errors.New("hook failed")
	})

	ctx := tc.InitiateTransfer(1, "test", 100, 1)
	if err := tc.StartDownload(1); err != nil {
		t.Fatalf("StartDownload failed: %v", err)
	}
	if err := tc.FileCompleted(1); err != nil {
		t.Fatalf("FileCompleted failed: %v", err)
	}

	// CompleteTransfer should succeed even if hook errors
	if err := tc.CompleteTransfer(1); err != nil {
		t.Fatalf("CompleteTransfer failed: %v", err)
	}
	if ctx.GetState() != TransferLifecycleProcessed {
		t.Fatalf("expected Processed state, got %s", ctx.GetState())
	}
}

func TestCoordinatorFailTransferWithCancelledError(t *testing.T) {
	m := newTestManager()
	tc := m.coordinator

	ctx := tc.InitiateTransfer(1, "test", 100, 1)

	cancelErr := NewDownloadCancelledError("file.mkv", "user requested")
	if err := tc.FailTransfer(1, cancelErr); err != nil {
		t.Fatalf("FailTransfer failed: %v", err)
	}

	if ctx.GetState() != TransferLifecycleCancelled {
		t.Fatalf("expected Cancelled state, got %s", ctx.GetState())
	}
	if ctx.GetError() == nil {
		t.Fatal("expected Error to be set")
	}
}

func TestCoordinatorFailTransferWithRegularError(t *testing.T) {
	m := newTestManager()
	tc := m.coordinator

	ctx := tc.InitiateTransfer(1, "test", 100, 1)

	regularErr := errors.New("network timeout")
	if err := tc.FailTransfer(1, regularErr); err != nil {
		t.Fatalf("FailTransfer failed: %v", err)
	}

	if ctx.GetState() != TransferLifecycleFailed {
		t.Fatalf("expected Failed state, got %s", ctx.GetState())
	}
	if ctx.GetError() == nil {
		t.Fatal("expected Error to be set")
	}
}

func TestCoordinatorFailTransferNotFound(t *testing.T) {
	m := newTestManager()
	tc := m.coordinator

	err := tc.FailTransfer(999, errors.New("some error"))
	if err == nil {
		t.Fatal("expected error for unknown transfer")
	}

	// Should be a TransferNotFound error
	var dlErr *DownloadError
	if !errors.As(err, &dlErr) || dlErr.Type != "TransferNotFound" {
		t.Fatalf("expected TransferNotFound error, got: %v", err)
	}
}

func TestCoordinatorCompleteTransferNotFound(t *testing.T) {
	m := newTestManager()
	tc := m.coordinator

	err := tc.CompleteTransfer(999)
	if err == nil {
		t.Fatal("expected error for unknown transfer")
	}
}

func TestCoordinatorCompleteTransferFromProcessedStateErrors(t *testing.T) {
	m := newTestManager()
	tc := m.coordinator

	ctx := tc.InitiateTransfer(1, "test", 100, 1)
	if err := tc.StartDownload(1); err != nil {
		t.Fatalf("StartDownload failed: %v", err)
	}
	if err := tc.FileCompleted(1); err != nil {
		t.Fatalf("FileCompleted failed: %v", err)
	}
	if err := tc.CompleteTransfer(1); err != nil {
		t.Fatalf("CompleteTransfer failed: %v", err)
	}

	if ctx.GetState() != TransferLifecycleProcessed {
		t.Fatalf("expected Processed, got %s", ctx.GetState())
	}

	// Calling CompleteTransfer again should error (Processed is not a valid source state)
	err := tc.CompleteTransfer(1)
	if err == nil {
		t.Fatal("expected error when completing from Processed state")
	}
}
