package download

import (
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
)

type temporaryOnlyError struct{}

func (temporaryOnlyError) Error() string   { return "temporary network error" }
func (temporaryOnlyError) Timeout() bool   { return false }
func (temporaryOnlyError) Temporary() bool { return true }

func TestIsTransientError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil_error",
			err:  nil,
			want: false,
		},
		{
			name: "download_cancelled_error",
			err:  NewDownloadCancelledError("test.mkv", "shutdown"),
			want: false,
		},
		{
			name: "connection_reset",
			err:  errors.New("connection reset"),
			want: true,
		},
		{
			name: "connection_refused",
			err:  errors.New("connection refused"),
			want: true,
		},
		{
			name: "io_timeout",
			err:  errors.New("i/o timeout"),
			want: true,
		},
		{
			name: "http_429_too_many_requests",
			err:  errors.New("HTTP 429 Too Many Requests"),
			want: true,
		},
		{
			name: "server_returned_503",
			err:  errors.New("server returned 503"),
			want: false,
		},
		{
			name: "bad_gateway_502",
			err:  errors.New("bad gateway 502"),
			want: true,
		},
		{
			name: "gateway_timeout_504",
			err:  errors.New("gateway timeout 504"),
			want: true,
		},
		{
			name: "unexpected_eof",
			err:  io.ErrUnexpectedEOF,
			want: true,
		},
		{
			name: "wrapped_unexpected_eof",
			err:  fmt.Errorf("download failed: %w", io.ErrUnexpectedEOF),
			want: true,
		},
		{
			name: "random_non_transient_error",
			err:  errors.New("some random error"),
			want: false,
		},
		{
			name: "numeric_movie_path",
			err:  errors.New("failed to open /downloads/500 Days of Summer/movie.mkv"),
			want: false,
		},
		{
			name: "deprecated_temporary_without_timeout",
			err:  temporaryOnlyError{},
			want: false,
		},
		{
			name: "wrapped_connection_reset",
			err:  fmt.Errorf("request failed: %w", errors.New("connection reset")),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isTransientError(tt.err)
			if got != tt.want {
				t.Errorf("isTransientError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestDownloadRetryDelayIsBounded(t *testing.T) {
	tests := []struct {
		attempt int
		delay   time.Duration
		ok      bool
	}{
		{attempt: 0, ok: false},
		{attempt: 1, delay: 30 * time.Second, ok: true},
		{attempt: 2, delay: time.Minute, ok: true},
		{attempt: 5, delay: 8 * time.Minute, ok: true},
		{attempt: 6, ok: false},
	}

	for _, tt := range tests {
		delay, ok := downloadRetryDelay(tt.attempt)
		if ok != tt.ok {
			t.Fatalf("downloadRetryDelay(%d) ok = %v, want %v", tt.attempt, ok, tt.ok)
		}
		if delay != tt.delay {
			t.Fatalf("downloadRetryDelay(%d) = %s, want %s", tt.attempt, delay, tt.delay)
		}
	}
}

func TestScheduleDownloadRetryRequeuesJob(t *testing.T) {
	manager := &Manager{
		jobs:     make(chan downloadJob, 1),
		stopChan: make(chan struct{}),
		running:  true,
		downloadRetryDelay: func(attempt int) (time.Duration, bool) {
			return 0, attempt == 1
		},
	}
	job := downloadJob{FileID: 11, Name: "example.mkv", TransferID: 22}

	if !manager.scheduleDownloadRetry(job, io.ErrUnexpectedEOF) {
		t.Fatal("expected transient failure to schedule a retry")
	}

	select {
	case got := <-manager.jobs:
		if got != job {
			t.Fatalf("requeued job = %#v, want %#v", got, job)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for requeued download")
	}
	manager.workerWg.Wait()

	value, ok := manager.downloadRetryAttempts.Load(job.FileID)
	if !ok || value.(int) != 1 {
		t.Fatalf("retry attempt = %v, present = %v; want 1, true", value, ok)
	}
}

func TestScheduleDownloadRetryStopsAtBound(t *testing.T) {
	manager := &Manager{
		jobs:     make(chan downloadJob, 1),
		stopChan: make(chan struct{}),
	}
	job := downloadJob{FileID: 11, Name: "example.mkv", TransferID: 22}
	manager.downloadRetryAttempts.Store(job.FileID, maxDownloadRetryRounds)

	if manager.scheduleDownloadRetry(job, io.ErrUnexpectedEOF) {
		t.Fatal("did not expect a retry after the configured bound")
	}
	if len(manager.jobs) != 0 {
		t.Fatal("did not expect an exhausted retry to enqueue a job")
	}
}

func TestScheduleDownloadRetryCancelsDuringShutdown(t *testing.T) {
	manager := &Manager{
		jobs:     make(chan downloadJob, 1),
		stopChan: make(chan struct{}),
		running:  true,
		downloadRetryDelay: func(int) (time.Duration, bool) {
			return time.Hour, true
		},
	}
	job := downloadJob{FileID: 11, Name: "example.mkv", TransferID: 22}

	if !manager.scheduleDownloadRetry(job, io.ErrUnexpectedEOF) {
		t.Fatal("expected retry to be scheduled")
	}
	close(manager.stopChan)
	manager.workerWg.Wait()

	if len(manager.jobs) != 0 {
		t.Fatal("did not expect a retry to be queued after shutdown")
	}
}

func TestQueueDownloadAfterStopDoesNotTouchClosedQueue(t *testing.T) {
	manager := &Manager{
		jobs:     make(chan downloadJob),
		stopChan: make(chan struct{}),
	}
	close(manager.jobs)

	manager.QueueDownload(downloadJob{FileID: 11, TransferID: 22})

	if _, ok := manager.activeFiles.Load(int64(11)); ok {
		t.Fatal("did not expect stopped manager to track a queued file")
	}
}
