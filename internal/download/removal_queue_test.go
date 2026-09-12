package download

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elsbrock/go-putio"
)

// These tests check lock independence, not filesystem latency. Removal writes
// durable markers, so allow slow storage while still catching deadlocks.
const removalWatchdog = 10 * time.Second

func awaitRemovalCondition(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.NewTimer(removalWatchdog)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal(message)
		}
	}
}

func TestRemovalDoesNotWaitForFullQueue(t *testing.T) {
	for _, sender := range []string{"processor", "delayed retry"} {
		for _, removedID := range []int64{101, 202} {
			t.Run(sender+"/"+map[int64]string{101: "same transfer", 202: "unrelated transfer"}[removedID], func(t *testing.T) {
				var startedDownloads atomic.Int32
				client := &fakeClient{
					files:       func(int64) ([]*putio.File, error) { return []*putio.File{{ID: 501, Name: "book.m4b", Size: 10}}, nil },
					downloadURL: func(context.Context, int64) (string, error) { startedDownloads.Add(1); return "", io.ErrUnexpectedEOF },
				}
				m := newManagerForTest(t, client)
				m.jobs = make(chan downloadJob, 1)
				m.jobs <- downloadJob{} // Full queue, with no worker to drain it.
				m.coordinator.InitiateTransfer(202, "unrelated", 502, 1)
				senderDone := make(chan struct{})
				if sender == "processor" {
					go func() {
						defer close(senderDone)
						m.processor.processTransfer(&putio.Transfer{ID: 101, Name: "Book", FileID: 500})
					}()
				} else {
					m.coordinator.InitiateTransfer(101, "Book", 500, 1)
					m.downloadRetryDelay = func(int) (time.Duration, bool) { return 0, true }
					if !m.scheduleDownloadRetry(downloadJob{FileID: 501, TransferID: 101, Name: "Book/book.m4b"}, io.ErrUnexpectedEOF) {
						t.Fatal("retry was not scheduled")
					}
					go func() { m.workerWg.Wait(); close(senderDone) }()
				}
				var removalDone chan struct{}
				var removalErr error
				var workerDone chan struct{}
				t.Cleanup(func() {
					close(m.stopChan)
					<-senderDone
					if workerDone != nil {
						<-workerDone
					}
					if removalDone != nil {
						<-removalDone
					}
				})
				awaitRemovalCondition(t, func() bool { return m.activeFileCount(101) == 1 }, "sender never claimed its blocked file")
				select {
				case <-senderDone:
					t.Fatal("sender was not blocked on the full queue")
				default:
				}
				removalDone = make(chan struct{})
				go func() {
					defer close(removalDone)
					_, removalErr = m.PrepareRemoval(removedID, false)
				}()
				select {
				case <-removalDone:
					if removalErr != nil {
						t.Fatal(removalErr)
					}
				case <-time.After(removalWatchdog):
					t.Fatal("removal waited for a full queue to drain")
				}
				// Remove the sender as well, then let its claimed job enter the
				// queue. Suppression must outlive the blocked channel send.
				if _, err := m.PrepareRemoval(101, false); err != nil {
					t.Fatal(err)
				}
				m.RemoveTransfer(101)
				if !m.RemovalPending(101) {
					t.Fatal("blocked sender lost suppression")
				}
				<-m.jobs
				<-senderDone
				workerDone = make(chan struct{})
				go func() { m.downloadWorker(); close(workerDone) }()
				awaitRemovalCondition(t, func() bool { return m.activeFileCount(101) == 0 }, "removed queued job did not drain")
				m.pruneRemovals(m.pendingRemovals(), nil)
				if m.RemovalPending(101) {
					t.Fatal("drained job retained suppression")
				}
				if startedDownloads.Load() != 0 {
					t.Fatal("removed queued job started a download")
				}
				if _, ok := m.GetTransferContext(101); ok {
					t.Fatal("queued job recreated removed transfer")
				}
			})
		}
	}
}

func TestTransientListingFailureDropsReservationForNextPoll(t *testing.T) {
	m := newManagerForTest(t, &fakeClient{files: func(int64) ([]*putio.File, error) { return nil, io.ErrUnexpectedEOF }})
	transfer := &putio.Transfer{ID: 101, Name: "Book", FileID: 500}
	m.processor.processTransfer(transfer)
	if _, exists := m.GetTransferContext(101); exists {
		t.Fatal("failed listing retained its Initial reservation")
	}
	if !m.processor.shouldProcess(transfer) {
		t.Fatal("failed listing cannot retry on next poll")
	}
}

func TestListingPublishesImmutableFileCountForConcurrentReaders(t *testing.T) {
	listing := make(chan struct{})
	release := make(chan struct{})
	client := &fakeClient{files: func(int64) ([]*putio.File, error) {
		close(listing)
		<-release
		return []*putio.File{{ID: 501, Name: "book.m4b", Size: 10}}, nil
	}}
	m := newManagerForTest(t, client)
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.processor.processTransfer(&putio.Transfer{ID: 101, Name: "Book", FileID: 500})
	}()
	releaseListing := sync.OnceFunc(func() { close(release) })
	t.Cleanup(func() { releaseListing(); <-done })
	<-listing
	reservation, exists := m.GetTransferContext(101)
	if !exists || reservation.TotalFiles != 0 {
		t.Fatal("missing Initial listing reservation")
	}
	stopReader := make(chan struct{})
	readerDone := make(chan struct{})
	var observed atomic.Int32
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stopReader:
				return
			default:
			}
			if ctx, exists := m.GetTransferContext(101); exists {
				// RPC reads this published field without acquiring ctx.mu.
				observed.Store(ctx.TotalFiles)
			}
		}
	}()
	t.Cleanup(func() { close(stopReader); <-readerDone })
	releaseListing()
	<-done
	current, exists := m.GetTransferContext(101)
	if !exists || current == reservation || current.TotalFiles != 1 || reservation.TotalFiles != 0 {
		t.Fatal("listing mutated published file count instead of replacing its reservation")
	}
	awaitRemovalCondition(t, func() bool { return observed.Load() == 1 }, "reader never observed initialized file count")
}

func TestRemovalInvalidatesBlockedRemoteListing(t *testing.T) {
	listing := make(chan struct{})
	release := make(chan struct{})
	client := &fakeClient{files: func(int64) ([]*putio.File, error) {
		close(listing)
		<-release
		return []*putio.File{{ID: 501, Name: "book.m4b", Size: 10}}, nil
	}}
	m := newManagerForTest(t, client)
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.processor.processTransfer(&putio.Transfer{ID: 101, Name: "Book", FileID: 500})
	}()
	releaseListing := sync.OnceFunc(func() { close(release) })
	var removalDone chan struct{}
	var removalErr error
	t.Cleanup(func() {
		releaseListing()
		<-done
		if removalDone != nil {
			<-removalDone
		}
	})
	<-listing
	removalDone = make(chan struct{})
	go func() {
		defer close(removalDone)
		_, removalErr = m.PrepareRemoval(101, false)
	}()
	select {
	case <-removalDone:
		if removalErr != nil {
			t.Fatal(removalErr)
		}
	case <-time.After(removalWatchdog):
		t.Fatal("removal waited for a remote listing")
	}
	m.pruneRemovals(m.pendingRemovals(), nil)
	if m.RemovalPending(101) {
		t.Fatal("confirmed absent marker was not reclaimed")
	}
	releaseListing()
	<-done
	if _, ok := m.GetTransferContext(101); ok {
		t.Fatal("old listing recreated removed transfer")
	}
	if _, ok := m.GetTransferFiles(101); ok {
		t.Fatal("old listing recreated manifest")
	}
	if len(m.jobs) != 0 || m.activeFileCount(101) != 0 {
		t.Fatal("old listing requeued removed transfer")
	}
}
