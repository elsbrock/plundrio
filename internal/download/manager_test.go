package download

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/config"
	"github.com/elsbrock/plundrio/internal/log"
)

// TestMain silences logging so test output shows test results, not the
// download manager's very chatty INFO stream.
func TestMain(m *testing.M) {
	log.SetLevel(log.LevelNone)
	os.Exit(m.Run())
}

// fakeClient is a PutioClient stub. Each field lets a test override one call;
// the zero value returns empty results without erroring.
type fakeClient struct {
	transfers      func() ([]*putio.Transfer, error)
	files          func(fileID int64) ([]*putio.File, error)
	folderContents func(folderID int64) ([]*putio.File, error)
	downloadURL    func(ctx context.Context, fileID int64) (string, error)
	deletedFiles   chan int64
}

func (f *fakeClient) GetTransfers(context.Context) ([]*putio.Transfer, error) {
	if f.transfers != nil {
		return f.transfers()
	}
	return nil, nil
}

func (f *fakeClient) GetAllTransferFiles(_ context.Context, fileID int64) ([]*putio.File, error) {
	if f.files != nil {
		return f.files(fileID)
	}
	return nil, nil
}

// GetFiles backs managedFolders' discovery of put.io category subfolders.
func (f *fakeClient) GetFiles(_ context.Context, folderID int64) ([]*putio.File, error) {
	if f.folderContents != nil {
		return f.folderContents(folderID)
	}
	return nil, nil
}

func (f *fakeClient) RetryTransfer(context.Context, int64) (*putio.Transfer, error) {
	return &putio.Transfer{}, nil
}

func (f *fakeClient) DeleteTransfer(context.Context, int64) error { return nil }

func (f *fakeClient) DeleteFile(_ context.Context, fileID int64) error {
	if f.deletedFiles != nil {
		f.deletedFiles <- fileID
	}
	return nil
}

func (f *fakeClient) GetDownloadURL(ctx context.Context, fileID int64) (string, error) {
	if f.downloadURL != nil {
		return f.downloadURL(ctx, fileID)
	}
	return "", nil
}

// newManagerForTest builds a Manager backed by client, rooted at a temp dir.
func newManagerForTest(t *testing.T, client PutioClient) *Manager {
	t.Helper()
	return New(&config.Config{
		TargetDir:   t.TempDir(),
		FolderID:    testFolderID,
		WorkerCount: 1,
	}, client)
}

const testFolderID = 42

// Regression test: checkTransfers republishes the transfer list from the
// monitor goroutine while torrent-get reads it from an HTTP handler
// goroutine. This previously raced on a plain map field. Meaningful under
// -race, which CI runs.
func TestGetTransfersIsRaceFreeWithCheckTransfers(t *testing.T) {
	client := &fakeClient{
		transfers: func() ([]*putio.Transfer, error) {
			return []*putio.Transfer{
				{ID: 1, Name: "a", Status: "DOWNLOADING", SaveParentID: testFolderID},
				{ID: 2, Name: "b", Status: "COMPLETED", SaveParentID: testFolderID},
				{ID: 3, Name: "c", Status: "ERROR", SaveParentID: 999}, // different folder
			}, nil
		},
	}
	m := newManagerForTest(t, client)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			m.processor.checkTransfers()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			for _, tr := range m.GetTransfers() {
				_ = tr.Name
			}
		}
	}()
	wg.Wait()
	m.processorWg.Wait()

	// Only transfers in the configured folder are published.
	got := m.GetTransfers()
	if len(got) != 2 {
		t.Fatalf("expected 2 transfers from the configured folder, got %d", len(got))
	}
	for _, tr := range got {
		if tr.SaveParentID != testFolderID {
			t.Errorf("transfer %d from foreign folder %d leaked into results", tr.ID, tr.SaveParentID)
		}
	}
}

// GetTransfers must hand back a copy so callers cannot mutate the published
// slice out from under the monitor.
func TestGetTransfersReturnsCopy(t *testing.T) {
	client := &fakeClient{
		transfers: func() ([]*putio.Transfer, error) {
			return []*putio.Transfer{
				{ID: 1, Name: "a", Status: "SEEDING", SaveParentID: testFolderID},
			}, nil
		},
	}
	m := newManagerForTest(t, client)
	m.processor.checkTransfers()
	m.processorWg.Wait()

	first := m.GetTransfers()
	if len(first) != 1 {
		t.Fatalf("expected 1 transfer, got %d", len(first))
	}
	first[0] = nil // caller scribbles on its copy

	second := m.GetTransfers()
	if len(second) != 1 || second[0] == nil {
		t.Fatal("mutating the returned slice affected the processor's copy")
	}
}

// No transfers yet (or none in our folder) must not report stale or bogus data.
func TestGetTransfersEmptyBeforeFirstPoll(t *testing.T) {
	m := newManagerForTest(t, &fakeClient{})
	if got := m.GetTransfers(); len(got) != 0 {
		t.Fatalf("expected no transfers before the first poll, got %d", len(got))
	}
}

// Regression test: Stop() used to close m.jobs, so a QueueDownload racing the
// shutdown panicked with "send on closed channel". A send on a closed channel
// panics even when the stopChan case of the select is also ready.
func TestQueueDownloadDuringStopDoesNotPanic(t *testing.T) {
	m := newManagerForTest(t, &fakeClient{})
	m.coordinator.InitiateTransfer(1, "test", 1, 5000)
	m.Start()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5000; i++ {
			m.QueueDownload(downloadJob{FileID: int64(i), TransferID: 1})
		}
	}()

	m.Stop()
	wg.Wait() // panics in the goroutine would fail the test run
}

// Regression test: QueueDownload used to hold m.mu across the blocking send on
// a full jobs queue, while Stop() needed the same mutex before it could close
// stopChan to release the sender — a shutdown deadlock whenever the queue
// filled up. Guarded by a timeout so a regression fails instead of hanging.
func TestStopDoesNotDeadlockOnFullQueue(t *testing.T) {
	// Park every worker inside GetDownloadURL so nothing drains the queue.
	// The workers stay parked until the manager's context is cancelled, which
	// is exactly what Stop does.
	parked := make(chan struct{}, 1)
	client := &fakeClient{
		downloadURL: func(ctx context.Context, _ int64) (string, error) {
			select {
			case parked <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return "", ctx.Err()
		},
	}
	m := newManagerForTest(t, client)
	m.Start()

	// Queue past the buffer (WorkerCount*BufferMultiple) so the sender blocks
	// on the channel send with no worker able to take it.
	m.coordinator.InitiateTransfer(1, "test", 1, 100)
	queuing := make(chan struct{})
	go func() {
		defer close(queuing)
		for i := 0; i < 100; i++ {
			m.QueueDownload(downloadJob{FileID: int64(i), TransferID: 1})
		}
	}()

	<-parked // a worker is stuck; the queue is backing up behind it

	stopped := make(chan struct{})
	go func() {
		m.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(15 * time.Second):
		t.Fatal("Stop deadlocked against a blocked QueueDownload")
	}

	select {
	case <-queuing:
	case <-time.After(15 * time.Second):
		t.Fatal("QueueDownload never unblocked after Stop")
	}
}

// Stop must be safe to call repeatedly and without a preceding Start, since
// main.go both defers it and calls it explicitly.
func TestStopIsIdempotent(t *testing.T) {
	m := newManagerForTest(t, &fakeClient{})
	m.Stop() // never started

	m.Start()
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Stop()
		m.Stop()
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("repeated Stop calls hung")
	}
}
