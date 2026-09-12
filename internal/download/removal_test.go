package download

import (
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/elsbrock/go-putio"
)

func TestRemovalReleasesMemoryAndSuppressesRestart(t *testing.T) {
	m := newManagerForTest(t, &fakeClient{})
	transfer := &putio.Transfer{ID: 101, Name: "Book", FileID: 501, Status: "COMPLETED"}
	m.SetCategory(101, "books")
	m.coordinator.InitiateTransfer(101, "Book", 501, 1)
	m.processor.retryAttempts.Store(int64(101), 2)
	m.processor.reprocessAttempts.Store(int64(101), 2)
	if err := m.transferFiles.Set(101, []TransferFile{{Name: "Book/book.m4b", Length: 10}}); err != nil {
		t.Fatal(err)
	}
	if category, err := m.PrepareRemoval(101, false); err != nil || category != "books" {
		t.Fatalf("prepare category = %q, error = %v", category, err)
	}
	if _, ok := m.GetTransferContext(101); ok {
		t.Fatal("context retained")
	}
	if len(m.categories.mapping) != 0 {
		t.Fatal("category cache retained")
	}
	if _, ok := m.processor.retryAttempts.Load(int64(101)); ok {
		t.Fatal("retry tracking retained")
	}
	if _, ok := m.processor.reprocessAttempts.Load(int64(101)); ok {
		t.Fatal("reprocess tracking retained")
	}
	if m.GetCategory(101) != "books" {
		t.Fatal("authoritative category lost")
	}
	if _, ok := m.GetTransferFiles(101); !ok {
		t.Fatal("manifest lost")
	}

	restarted := New(m.cfg, &fakeClient{})
	restarted.categories.Load()
	if restarted.GetCategory(101) != "books" || len(restarted.categories.mapping) != 0 {
		t.Fatal("restart lost category or reloaded active tracking")
	}
	if restarted.processor.shouldProcess(transfer) {
		t.Fatal("removed transfer eligible after restart")
	}
	restarted.processor.processTransfer(transfer)
	if _, ok := restarted.GetTransferContext(101); ok {
		t.Fatal("restart recreated context")
	}
	job := downloadJob{FileID: 501, TransferID: 101}
	restarted.QueueDownload(job)
	if restarted.scheduleDownloadRetry(job, io.ErrUnexpectedEOF) {
		t.Fatal("removed transfer retried")
	}
	if len(restarted.jobs) != 0 {
		t.Fatal("removed transfer queued")
	}
}

func TestRemovalPrunesOnlyAfterSuccessfulFullListing(t *testing.T) {
	var transfers []*putio.Transfer
	var listErr error
	client := &fakeClient{transfers: func() ([]*putio.Transfer, error) { return transfers, listErr }}
	m := newManagerForTest(t, client)
	if err := m.transferFiles.Set(101, []TransferFile{{Name: "Book/book.m4b", Length: 10}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PrepareRemoval(101, false); err != nil {
		t.Fatal(err)
	}
	listErr = errors.New("unavailable")
	m.processor.checkTransfers()
	if !m.RemovalPending(101) {
		t.Fatal("failed listing discarded marker")
	}
	listErr = nil
	transfers = []*putio.Transfer{{ID: 101, SaveParentID: testFolderID + 1, Status: "COMPLETED"}}
	m.processor.checkTransfers()
	if !m.RemovalPending(101) {
		t.Fatal("folder move discarded marker")
	}
	transfers = nil
	m.processor.checkTransfers()
	if m.RemovalPending(101) {
		t.Fatal("confirmed remote absence retained marker")
	}
	if _, ok := m.GetTransferFiles(101); ok {
		t.Fatal("confirmed remote absence retained manifest")
	}
}

func TestRemovalKeepsSuppressionUntilActiveWorkerDrains(t *testing.T) {
	deleted := make(chan int64, 1)
	m := newManagerForTest(t, &fakeClient{deletedFiles: deleted})
	m.coordinator.InitiateTransfer(101, "Book", 501, 1)
	m.activeFiles.Store(int64(501), int64(101))
	if err := m.transferFiles.Set(101, []TransferFile{{Name: "Book/book.m4b", Length: 10}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PrepareRemoval(101, false); err != nil {
		t.Fatal(err)
	}
	m.RemoveTransfer(101)
	if !m.RemovalPending(101) {
		t.Fatal("active worker lost suppression")
	}
	// A late completion must not recreate its context or clean up source data.
	m.handleFileCompletion(101, 501)
	if _, ok := m.GetTransferContext(101); ok {
		t.Fatal("late completion recreated context")
	}
	select {
	case id := <-deleted:
		t.Fatalf("late completion deleted source %d", id)
	default:
	}
	m.pruneRemovals(m.pendingRemovals(), nil)
	if m.RemovalPending(101) {
		t.Fatal("drained worker retained suppression after confirmed absence")
	}
}

func TestRemovalInvalidatesScheduledRetryAfterMarkerReclaimed(t *testing.T) {
	m := newManagerForTest(t, &fakeClient{})
	m.downloadRetryDelay = func(int) (time.Duration, bool) { return 100 * time.Millisecond, true }
	m.coordinator.InitiateTransfer(101, "Book", 501, 1)
	job := downloadJob{FileID: 501, TransferID: 101}
	if !m.scheduleDownloadRetry(job, io.ErrUnexpectedEOF) {
		t.Fatal("retry not scheduled")
	}
	if _, err := m.PrepareRemoval(101, false); err != nil {
		t.Fatal(err)
	}
	m.pruneRemovals(m.pendingRemovals(), nil)
	if m.RemovalPending(101) {
		t.Fatal("expected confirmed absent marker reclaimed")
	}
	m.workerWg.Wait()
	if len(m.jobs) != 0 || m.activeFileCount(101) != 0 {
		t.Fatal("delayed retry resurrected removed transfer")
	}
	if _, ok := m.downloadRetryAttempts.Load(int64(501)); ok {
		t.Fatal("delayed retry retained bookkeeping")
	}
}

func TestUnreadableRemovalStateFailsClosed(t *testing.T) {
	m := newManagerForTest(t, &fakeClient{})
	if _, err := m.PrepareRemoval(101, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.removalPath(101), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if !m.RemovalPending(101) {
		t.Fatal("corrupt marker ignored")
	}
	if _, err := m.PrepareRemoval(101, false); err == nil {
		t.Fatal("corrupt ownership state allowed destructive retry")
	}
}

func TestNullRemovalCategoryFailsClosed(t *testing.T) {
	m := newManagerForTest(t, &fakeClient{})
	if _, err := m.PrepareRemoval(101, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.removalPath(101), []byte("null"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PrepareRemoval(101, false); err == nil {
		t.Fatal("null ownership category allowed destructive retry")
	}
}
