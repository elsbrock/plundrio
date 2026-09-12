package download

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/api"
)

func markReviewForTest(t *testing.T, m *Manager, transfer *putio.Transfer) {
	t.Helper()
	m.removalMu.RLock()
	err := m.markNeedsReview(transfer)
	m.removalMu.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
}

func TestReadyRemovalRequiresAtomicClassification(t *testing.T) {
	for _, state := range []TransferLifecycleState{TransferLifecycleInitial, TransferLifecycleFailed, TransferLifecycleProcessed} {
		t.Run(state.String(), func(t *testing.T) {
			m := newManagerForTest(t, &fakeClient{})
			ctx := NewTransferContext(101, 0, state)
			m.coordinator.transfers.Store(int64(101), ctx)
			if state == TransferLifecycleFailed {
				// Reprocessing discards Failed before reserving the next Initial
				// context. Removal must recheck that gap under its own lock.
				if !m.processor.shouldProcess(&putio.Transfer{ID: 101}) {
					t.Fatal("failed retry not reserved")
				}
			}
			_, err := m.PrepareRemoval(101, true)
			if state == TransferLifecycleProcessed {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := m.PrepareRemoval(101, true); err != nil {
					t.Fatal("pending ordinary retry was blocked", err)
				}
			} else if err == nil || m.RemovalPending(101) {
				t.Fatal("unclassified generation accepted/mutated removal")
			}
		})
	}
}

func TestReviewSurvivesRestartAndRemoteChanges(t *testing.T) {
	for _, fileID := range []int64{0, 501} {
		t.Run(fmt.Sprint(fileID), func(t *testing.T) {
			client := &fakeClient{files: func(int64) ([]*putio.File, error) {
				t.Fatal("review restoration queried source")
				return nil, nil
			}}
			m := newManagerForTest(t, client)
			original := &putio.Transfer{ID: 101, Name: "Book", FileID: fileID, SaveParentID: testFolderID}
			m.SetCategory(101, "books")
			markReviewForTest(t, m, original)
			info, err := os.Stat(m.reviewPath(101))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("marker permissions: %v, %v", info, err)
			}
			for _, status := range []string{"ERROR", "COMPLETED", "DOWNLOADING"} {
				restarted := New(m.cfg, client)
				changed := *original
				changed.Status, changed.FileID = status, 999
				if !restarted.restoreReview(&changed) {
					t.Fatal("durable hold not restored")
				}
				ctx, ok := restarted.GetTransferContext(101)
				if !ok || ctx.GetState() != TransferLifecycleNeedsReview || ctx.TotalFiles != 0 || ctx.GetError() != nil || ctx.FileID != fileID {
					t.Fatalf("unexpected restored context: %+v", ctx)
				}
				if restarted.processor.shouldProcess(&changed) {
					t.Fatal("review became retryable")
				}
				if _, err := restarted.PrepareRemoval(101, false); err == nil {
					t.Fatal("generic removal accepted review")
				}
			}
		})
	}
}

func TestInvalidReviewMarkerSuspendsProcessing(t *testing.T) {
	for _, content := range []string{"{", "null", "{}", `{"id":102,"name":"Book"}`} {
		t.Run(content, func(t *testing.T) {
			m := newManagerForTest(t, &fakeClient{})
			if err := os.MkdirAll(m.transferFiles.stateDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(m.reviewPath(101), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			transfer := &putio.Transfer{ID: 101, Name: "Book", Status: "ERROR"}
			if !m.NeedsReview(101) || !m.restoreReview(transfer) {
				t.Fatal("malformed state did not retain hold")
			}
			if ctx, _ := m.GetTransferContext(101); ctx.GetState() != TransferLifecycleNeedsReview {
				t.Fatal("malformed review became retryable failure")
			}
			if _, err := m.PrepareReviewRetirement(context.Background(), transfer); err == nil {
				t.Fatal("malformed review authorized retirement")
			}
		})
	}
}

func TestReviewStorageFailureKeepsInMemoryHold(t *testing.T) {
	m := newManagerForTest(t, &fakeClient{})
	if err := os.WriteFile(m.transferFiles.stateDir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	transfer := &putio.Transfer{ID: 101, Name: "Book", SaveParentID: 42}
	m.SetCategory(101, "books")
	m.removalMu.RLock()
	err := m.markNeedsReview(transfer)
	m.removalMu.RUnlock()
	if err == nil {
		t.Fatal("broken state storage was accepted")
	}
	ctx, ok := m.GetTransferContext(101)
	if !ok || ctx.GetState() != TransferLifecycleNeedsReview || ctx.GetError() != nil {
		t.Fatal("storage failure lost operator hold")
	}
	if _, err := m.PrepareRemoval(101, false); err == nil {
		t.Fatal("broken review storage allowed generic deletion")
	}
	if err := os.Remove(m.transferFiles.stateDir); err != nil {
		t.Fatal(err)
	}
	if m.NeedsReview(101) {
		t.Fatal("test should now have only an in-memory hold")
	}
	if _, err := m.PrepareRemoval(101, false); err == nil {
		t.Fatal("in-memory review allowed generic deletion after storage repair")
	}
	changed := *transfer
	changed.SaveParentID, changed.FileID, changed.Name = 99, 999, "Renamed"
	m.SetCategory(101, "changed")
	if !m.restoreReview(&changed) || !m.NeedsReview(101) {
		t.Fatal("next poll did not persist the hold after storage repair")
	}
	review, err := m.loadReview(101)
	if err != nil || review.SaveParentID != 42 || review.FileID != 0 || review.Name != "Book" || review.Category != "books" {
		t.Fatalf("storage retry adopted changed identity: %+v, %v", review, err)
	}
	if !New(m.cfg, &fakeClient{}).restoreReview(transfer) {
		t.Fatal("repaired hold did not survive restart")
	}
}

func TestReviewRetirementRejectsChangedSourceAndManifest(t *testing.T) {
	for _, scenario := range []string{"source-returned", "transient-error", "child-not-found", "wrong-root", "renamed", "moved", "missing", "manifest", "corrupt-manifest", "dangling-manifest", "active-worker"} {
		t.Run(scenario, func(t *testing.T) {
			transfer := &putio.Transfer{ID: 101, Name: "Book", FileID: 501, SaveParentID: testFolderID, Status: "COMPLETED"}
			current := *transfer
			client := &fakeClient{transfers: func() ([]*putio.Transfer, error) {
				if scenario == "missing" {
					return nil, nil
				}
				return []*putio.Transfer{&current}, nil
			}, files: func(id int64) ([]*putio.File, error) {
				switch scenario {
				case "source-returned":
					return []*putio.File{{ID: id}}, nil
				case "transient-error":
					return nil, errors.New("unavailable")
				case "child-not-found":
					return nil, fmt.Errorf("list child: %w", &putio.ErrorResponse{Type: "NotFound"})
				case "wrong-root":
					return nil, &api.TransferSourceNotFoundError{FileID: 999, Err: &putio.ErrorResponse{Type: "NotFound"}}
				default:
					return nil, &api.TransferSourceNotFoundError{FileID: id, Err: &putio.ErrorResponse{Type: "NotFound"}}
				}
			}}
			m := newManagerForTest(t, client)
			markReviewForTest(t, m, transfer)
			switch scenario {
			case "renamed":
				current.Name = "Other"
			case "moved":
				current.SaveParentID++
			case "manifest":
				if err := m.transferFiles.Set(101, []TransferFile{{Name: "Book/file", Length: 1}}); err != nil {
					t.Fatal(err)
				}
			case "corrupt-manifest":
				if err := os.WriteFile(m.transferFiles.path(101), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "dangling-manifest":
				if err := os.Symlink(filepath.Join(m.cfg.TargetDir, "missing"), m.transferFiles.path(101)); err != nil {
					t.Fatal(err)
				}
			case "active-worker":
				m.activeFiles.Store(int64(501), int64(101))
			}
			if _, err := m.PrepareReviewRetirement(context.Background(), transfer); err == nil {
				t.Fatal("unsafe retirement accepted")
			}
			if m.RemovalPending(101) || !m.NeedsReview(101) {
				t.Fatal("rejected retirement changed durable state")
			}
		})
	}
}

func TestReviewRetirementKeepsRecordOnlyModeAcrossFailureAndRestart(t *testing.T) {
	for _, fileID := range []int64{0, 501} {
		t.Run(fmt.Sprint(fileID), func(t *testing.T) {
			transfer := &putio.Transfer{ID: 101, Name: "Book", FileID: fileID, SaveParentID: testFolderID, Status: "COMPLETED"}
			calls := 0
			client := &fakeClient{transfers: func() ([]*putio.Transfer, error) { return []*putio.Transfer{transfer}, nil }, files: func(id int64) ([]*putio.File, error) {
				calls++
				if id == 0 {
					t.Fatal("queried root")
				}
				return nil, fmt.Errorf("wrapped: %w", &api.TransferSourceNotFoundError{FileID: id, Err: &putio.ErrorResponse{Type: "NotFound"}})
			}}
			m := newManagerForTest(t, client)
			m.SetCategory(101, "books")
			markReviewForTest(t, m, transfer)
			for attempt := 0; attempt < 2; attempt++ {
				category, err := m.PrepareReviewRetirement(context.Background(), transfer)
				if err != nil || category != "books" {
					t.Fatalf("prepare: %q, %v", category, err)
				}
				if !m.RemovalPending(101) || !m.NeedsReview(101) {
					t.Fatal("retirement lost record-only suppression")
				}
				if _, ok := m.GetTransferContext(101); ok {
					t.Fatal("pending retirement retained context")
				}
				if _, err := m.PrepareRemoval(101, false); err == nil {
					t.Fatal("generic retry escaped record-only mode")
				}
				m = New(m.cfg, client)
				if !m.restoreReview(transfer) {
					t.Fatal("pending review disappeared after restart")
				}
				if _, ok := m.GetTransferContext(101); ok {
					t.Fatal("pending retirement recreated context")
				}
			}
			if fileID == 0 && calls != 0 {
				t.Fatal("zero source queried")
			}
		})
	}
}

func TestReviewRetirementRevalidatesAfterNetwork(t *testing.T) {
	transfer := &putio.Transfer{ID: 101, Name: "Book", FileID: 501, Status: "COMPLETED"}
	var m *Manager
	client := &fakeClient{transfers: func() ([]*putio.Transfer, error) { return []*putio.Transfer{transfer}, nil }, files: func(id int64) ([]*putio.File, error) {
		// Also proves the network call does not hold removalMu.
		m.removalMu.Lock()
		defer m.removalMu.Unlock()
		if err := m.transferFiles.Set(101, []TransferFile{{Name: "Book/file", Length: 1}}); err != nil {
			t.Fatal(err)
		}
		return nil, &api.TransferSourceNotFoundError{FileID: id, Err: &putio.ErrorResponse{Type: "NotFound"}}
	}}
	m = newManagerForTest(t, client)
	markReviewForTest(t, m, transfer)
	if _, err := m.PrepareReviewRetirement(context.Background(), transfer); err == nil {
		t.Fatal("manifest added during network calls was ignored")
	}
	if m.RemovalPending(101) {
		t.Fatal("revalidation failure created removal marker")
	}
}
