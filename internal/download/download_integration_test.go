package download

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// tickInterval is the progress-ticker period used by these tests. Handlers
// pace their writes against it so the ticker genuinely fires mid-download —
// otherwise a test would pass simply because no bytes were ever reported.
const tickInterval = 5 * time.Millisecond

// writeSlowly sends size bytes in chunks, pausing long enough between them
// that the progress ticker fires several times.
func writeSlowly(w http.ResponseWriter, size int) {
	const chunks = 4
	chunk := make([]byte, size/chunks)
	for i := 0; i < chunks; i++ {
		_, _ = w.Write(chunk)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(10 * tickInterval)
	}
}

// serveThenTruncate announces contentLength but slowly writes only half the
// body before hanging up, so grab fails partway through with bytes already
// reported to the transfer total.
func serveThenTruncate(contentLength int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(contentLength))
		w.WriteHeader(http.StatusOK)
		writeSlowly(w, contentLength/2)
		// Returning with an unfulfilled Content-Length truncates the body.
	}
}

// requireProgressWasReported guards against a vacuous test: if the ticker
// never fired, there were no bytes to double-count in the first place.
func requireProgressWasReported(t *testing.T, state *DownloadState) {
	t.Helper()
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.downloaded == 0 {
		t.Fatal("no progress was ever reported; the test would pass vacuously")
	}
}

// newDownloadManager wires a Manager whose downloads point at srvURL and whose
// progress ticker fires fast enough for a test.
func newDownloadManager(t *testing.T, srvURL string) *Manager {
	t.Helper()
	m := newManagerForTest(t, &fakeClient{
		downloadURL: func(context.Context, int64) (string, error) { return srvURL, nil },
	})
	m.dlConfig.ProgressUpdateInterval = tickInterval
	m.ctx, m.cancel = context.WithCancel(context.Background())
	t.Cleanup(m.cancel)
	return m
}

// startTransfer registers a single-file transfer of the given size.
func startTransfer(t *testing.T, m *Manager, size int64) *TransferContext {
	t.Helper()
	transferCtx := m.coordinator.InitiateTransfer(1, "movie", 100, 1)
	transferCtx.SetTotalSize(size)
	if err := m.coordinator.StartDownload(1); err != nil {
		t.Fatalf("StartDownload: %v", err)
	}
	return transferCtx
}

// Regression test: a failed attempt used to leave the bytes it had already
// reported in the transfer's running total, while the retry restarted its own
// accounting from zero. Two attempts therefore counted the same bytes twice
// and progress could climb past 100%.
func TestFailedAttemptDoesNotLeaveBytesBehind(t *testing.T) {
	const size = 1 << 20
	srv := httptest.NewServer(serveThenTruncate(size))
	defer srv.Close()

	m := newDownloadManager(t, srv.URL)
	transferCtx := startTransfer(t, m, size)

	state := &DownloadState{FileID: 10, Name: "movie.mkv", TransferID: 1, StartTime: time.Now()}
	if err := m.downloadFile(state); err == nil {
		t.Fatal("expected the truncated download to fail")
	}

	// The attempt must actually have reported bytes, otherwise there would be
	// nothing to double-count and the assertion below would prove nothing.
	requireProgressWasReported(t, state)
	reported, _, _, _ := transferCtx.GetProgress()
	if reported == 0 {
		t.Fatal("the failed attempt reported no bytes to the transfer total")
	}

	m.rollbackAttemptBytes(state)

	downloaded, _, _, _ := transferCtx.GetProgress()
	if downloaded != 0 {
		t.Errorf("failed attempt left %d bytes in the transfer total, want 0", downloaded)
	}
}

// A successful download must report exactly the file's size — no more (the
// double-count bug) and no less (a missed final flush).
func TestSuccessfulDownloadCountsBytesExactlyOnce(t *testing.T) {
	const size = 1 << 20
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		w.WriteHeader(http.StatusOK)
		writeSlowly(w, size)
	}))
	defer srv.Close()

	m := newDownloadManager(t, srv.URL)
	transferCtx := startTransfer(t, m, size)

	state := &DownloadState{FileID: 10, Name: "movie.mkv", TransferID: 1, StartTime: time.Now()}
	if err := m.downloadWithRetry(state); err != nil {
		t.Fatalf("download failed: %v", err)
	}

	downloaded, total, _, _ := transferCtx.GetProgress()
	if downloaded != size {
		t.Errorf("counted %d bytes for a %d byte file", downloaded, size)
	}
	if downloaded > total {
		t.Errorf("reported progress exceeds 100%%: %d/%d", downloaded, total)
	}

	// And the file actually landed on disk.
	written := filepath.Join(m.cfg.TargetDir, "movie.mkv")
	info, err := os.Stat(written)
	if err != nil {
		t.Fatalf("expected the download on disk: %v", err)
	}
	if info.Size() != size {
		t.Errorf("wrote %d bytes to disk, want %d", info.Size(), size)
	}
}

// A retried download must also end up counting the file exactly once, even
// though the first attempt already reported part of it.
func TestRetriedDownloadCountsBytesExactlyOnce(t *testing.T) {
	const size = 1 << 20
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		w.WriteHeader(http.StatusOK)
		if attempts.Add(1) == 1 {
			writeSlowly(w, size/2) // first attempt truncates partway through
			return
		}
		writeSlowly(w, size)
	}))
	defer srv.Close()

	m := newDownloadManager(t, srv.URL)
	transferCtx := startTransfer(t, m, size)

	state := &DownloadState{FileID: 10, Name: "movie.mkv", TransferID: 1, StartTime: time.Now()}

	// Drive the two attempts explicitly: whether a truncated body is
	// classified as transient is isTransientError's business, not this test's.
	if err := m.downloadFile(state); err == nil {
		t.Fatal("expected the first attempt to fail")
	}
	requireProgressWasReported(t, state)
	m.rollbackAttemptBytes(state)

	if err := m.downloadFile(state); err != nil {
		t.Fatalf("second attempt failed: %v", err)
	}

	downloaded, _, _, _ := transferCtx.GetProgress()
	if downloaded != size {
		t.Errorf("after a retry, counted %d bytes for a %d byte file", downloaded, size)
	}
}

// End-to-end through downloadWithRetry: a truncated first attempt is retried
// and the file is still counted exactly once. This covers the retry path
// actually performing the rollback, which the narrower tests above cannot.
func TestRetryThroughDownloadWithRetryCountsBytesOnce(t *testing.T) {
	const size = 1 << 20
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		w.WriteHeader(http.StatusOK)
		if attempts.Add(1) == 1 {
			writeSlowly(w, size/2) // truncated: grab reports "unexpected EOF"
			return
		}
		writeSlowly(w, size)
	}))
	defer srv.Close()

	m := newDownloadManager(t, srv.URL)
	transferCtx := startTransfer(t, m, size)

	state := &DownloadState{FileID: 10, Name: "movie.mkv", TransferID: 1, StartTime: time.Now()}
	if err := m.downloadWithRetry(state); err != nil {
		t.Fatalf("download should have succeeded on retry: %v", err)
	}

	if got := attempts.Load(); got < 2 {
		t.Fatalf("expected the truncated attempt to be retried, saw %d attempt(s)", got)
	}

	downloaded, total, _, _ := transferCtx.GetProgress()
	if downloaded != size {
		t.Errorf("counted %d bytes for a %d byte file across a retry", downloaded, size)
	}
	if downloaded > total {
		t.Errorf("progress exceeds 100%%: %d/%d", downloaded, total)
	}
}

// A stalled download must be abandoned rather than pinning a worker forever:
// grab itself applies no timeout.
func TestRemovedStalledWorkerDrainsWithoutRetryOrSourceCleanup(t *testing.T) {
	const size = 1 << 20
	started := make(chan struct{}, 1)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Length", strconv.Itoa(size))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 1024))
		w.(http.Flusher).Flush()
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer srv.Close()
	m := newDownloadManager(t, srv.URL)
	m.dlConfig.DownloadStallTimeout = 100 * time.Millisecond
	deleted := make(chan int64, 1)
	m.client.(*fakeClient).deletedFiles = deleted
	startTransfer(t, m, size)
	if err := m.transferFiles.Set(1, []TransferFile{{Name: "movie/movie.mkv", Length: size}}); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() { m.downloadWorker(); close(stopped) }()
	defer func() { close(m.stopChan); <-stopped }()
	m.QueueDownload(downloadJob{FileID: 10, TransferID: 1, Name: "movie.mkv"})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("download never started")
	}
	if _, err := m.PrepareRemoval(1, false); err != nil {
		t.Fatal(err)
	}
	m.RemoveTransfer(1)
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for m.activeFileCount(1) != 0 {
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatal("removed stalled worker did not drain")
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("removed worker retried %d HTTP requests", requests.Load())
	}
	if _, ok := m.GetTransferContext(1); ok {
		t.Fatal("removed worker recreated transfer")
	}
	if !m.RemovalPending(1) {
		t.Fatal("worker discarded suppression before remote absence was confirmed")
	}
	if _, ok := m.GetTransferFiles(1); !ok {
		t.Fatal("worker discarded ownership evidence")
	}
	select {
	case id := <-deleted:
		t.Fatalf("removed worker deleted source %d", id)
	default:
	}
	m.pruneRemovals(m.pendingRemovals(), nil)
	if m.RemovalPending(1) {
		t.Fatal("drained worker metadata not reclaimed")
	}
}

func TestStalledDownloadIsCancelled(t *testing.T) {
	const size = 1 << 20
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 1024))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-stop // then go quiet, without closing the connection
	}))
	defer srv.Close()
	defer close(stop)

	m := newDownloadManager(t, srv.URL)
	m.dlConfig.DownloadStallTimeout = 50 * time.Millisecond
	startTransfer(t, m, size)

	state := &DownloadState{FileID: 10, Name: "movie.mkv", TransferID: 1, StartTime: time.Now()}
	done := make(chan error, 1)
	go func() { done <- m.downloadFile(state) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected the stalled download to fail")
		}
		if !errors.Is(err, errDownloadStalled) {
			t.Fatalf("stalled download error = %v, want errDownloadStalled", err)
		}
		var downloadErr *DownloadError
		if errors.As(err, &downloadErr) && downloadErr.Type == "DownloadCancelled" {
			t.Fatalf("stalled download was misclassified as shutdown cancellation: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("stalled download was never cancelled")
	}
}
