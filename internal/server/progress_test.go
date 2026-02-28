package server

import (
	"testing"

	"github.com/elsbrock/plundrio/internal/download"
)

// newTestTransferCtx creates a TransferContext for testing with the given parameters.
func newTestTransferCtx(state download.TransferLifecycleState, totalFiles int32, completedFiles int32, totalSize int64, downloadedSize int64) *download.TransferContext {
	ctx := download.NewTransferContext(0, totalFiles, state)
	ctx.SetTotalSize(totalSize)
	ctx.AddDownloadedBytes(downloadedSize)
	// Simulate completed files by calling no internal method — we just need the
	// progress snapshot to return the right completedFiles count.
	// Since completedFiles is unexported, we rely on the fact that GetProgress()
	// returns 0 for completedFiles by default. For tests that need nonzero
	// completedFiles we must use a different approach.
	//
	// Actually, we can work around this: the progress calculation uses GetProgress()
	// which returns completedFiles. We need a way to set it. Let's just build the
	// tests to not depend on completedFiles from GetProgress when we can use
	// totalSize/downloadedSize instead (which is the primary path).
	//
	// For the file-count fallback path (totalSize==0), we accept that completedFiles
	// will be 0 in cross-package tests. The coordinator_test.go (same package) covers
	// the completedFiles path.
	return ctx
}

func TestCalculateProgress(t *testing.T) {
	tests := []struct {
		name              string
		input             progressInput
		wantPercentDone   float64
		wantStatus        int
		wantLeftUntilDone int64
	}{
		// ---------------------------------------------------------------
		// No context, put.io only — maps 0–100% → 0–50%
		// ---------------------------------------------------------------
		{
			name: "no context, putio 0%",
			input: progressInput{
				PutioPercentDone: 0,
				PutioStatus:      "DOWNLOADING",
				PutioSize:        1000,
			},
			wantPercentDone:   0.0,
			wantStatus:        trStatusDownload,
			wantLeftUntilDone: 1000,
		},
		{
			name: "no context, putio 50%",
			input: progressInput{
				PutioPercentDone: 50,
				PutioStatus:      "DOWNLOADING",
				PutioSize:        1000,
			},
			wantPercentDone:   0.25,
			wantStatus:        trStatusDownload,
			wantLeftUntilDone: 500,
		},
		{
			name: "no context, putio 100%",
			input: progressInput{
				PutioPercentDone: 100,
				PutioStatus:      "DOWNLOADING",
				PutioSize:        1000,
			},
			wantPercentDone:   0.5,
			wantStatus:        trStatusDownload,
			wantLeftUntilDone: 0,
		},
		// ---------------------------------------------------------------
		// No context, terminal put.io statuses → 100%
		// ---------------------------------------------------------------
		{
			name: "no context, COMPLETED status",
			input: progressInput{
				PutioPercentDone: 100,
				PutioStatus:      "COMPLETED",
				PutioSize:        1000,
			},
			wantPercentDone:   1.0,
			wantStatus:        trStatusSeed,
			wantLeftUntilDone: 0,
		},
		{
			name: "no context, SEEDING status",
			input: progressInput{
				PutioPercentDone: 100,
				PutioStatus:      "SEEDING",
				PutioSize:        1000,
			},
			wantPercentDone:   1.0,
			wantStatus:        trStatusSeed,
			wantLeftUntilDone: 0,
		},
		// ---------------------------------------------------------------
		// With context, partial local download (50/50 split)
		// ---------------------------------------------------------------
		{
			name: "with context, putio 100% + local 50%",
			input: progressInput{
				PutioPercentDone: 100,
				PutioStatus:      "COMPLETED",
				PutioSize:        1000,
				TransferCtx:      newTestTransferCtx(download.TransferLifecycleDownloading, 2, 1, 1000, 500),
			},
			wantPercentDone:   0.75, // 0.5 (putio) + 0.25 (local 500/1000 * 0.5)
			wantStatus:        trStatusDownload,
			wantLeftUntilDone: 500, // 0 putio + 500 local
		},
		{
			name: "with context, putio 50% + local 0%",
			input: progressInput{
				PutioPercentDone: 50,
				PutioStatus:      "DOWNLOADING",
				PutioSize:        2000,
				TransferCtx:      newTestTransferCtx(download.TransferLifecycleDownloading, 4, 0, 2000, 0),
			},
			wantPercentDone:   0.25, // 0.25 (putio) + 0.0 (local)
			wantStatus:        trStatusDownload,
			wantLeftUntilDone: 3000, // 1000 putio + 2000 local
		},
		// ---------------------------------------------------------------
		// With context, processed state → 100%, status=seed
		// ---------------------------------------------------------------
		{
			name: "with context, processed state",
			input: progressInput{
				PutioPercentDone: 100,
				PutioStatus:      "COMPLETED",
				PutioSize:        1000,
				TransferCtx:      newTestTransferCtx(download.TransferLifecycleProcessed, 3, 3, 1000, 1000),
			},
			wantPercentDone:   1.0,
			wantStatus:        trStatusSeed,
			wantLeftUntilDone: 0,
		},
		// ---------------------------------------------------------------
		// With context, completed state → uses mapPutioStatus
		// ---------------------------------------------------------------
		{
			name: "with context, completed state + COMPLETED status",
			input: progressInput{
				PutioPercentDone: 100,
				PutioStatus:      "COMPLETED",
				PutioSize:        1000,
				TransferCtx:      newTestTransferCtx(download.TransferLifecycleCompleted, 2, 2, 1000, 1000),
			},
			wantPercentDone:   1.0, // 0.5 + 0.5
			wantStatus:        trStatusSeed,
			wantLeftUntilDone: 0,
		},
		{
			name: "with context, completed state + IN_QUEUE status",
			input: progressInput{
				PutioPercentDone: 100,
				PutioStatus:      "IN_QUEUE",
				PutioSize:        1000,
				TransferCtx:      newTestTransferCtx(download.TransferLifecycleCompleted, 1, 1, 1000, 1000),
			},
			wantPercentDone:   1.0,
			wantStatus:        trStatusDownloadWaiting,
			wantLeftUntilDone: 0,
		},
		// ---------------------------------------------------------------
		// Zero size falls back to file count progress
		// Note: completedFiles is 0 from cross-package constructor,
		// so local progress is 0 (not 0.25 as with same-package access).
		// The file-count path is tested more thoroughly in coordinator_test.go.
		// ---------------------------------------------------------------
		{
			name: "with context, zero size uses file count (cross-package: completedFiles=0)",
			input: progressInput{
				PutioPercentDone: 100,
				PutioStatus:      "COMPLETED",
				PutioSize:        0,
				TransferCtx:      newTestTransferCtx(download.TransferLifecycleDownloading, 4, 0, 0, 0),
			},
			wantPercentDone:   0.5, // 0.5 (putio) + 0.0 (0/4 * 0.5)
			wantStatus:        trStatusDownload,
			wantLeftUntilDone: 0, // both putio and local are 0 bytes
		},
		// ---------------------------------------------------------------
		// Edge cases
		// ---------------------------------------------------------------
		{
			name: "with context, zero size + zero files",
			input: progressInput{
				PutioPercentDone: 50,
				PutioStatus:      "DOWNLOADING",
				PutioSize:        1000,
				// TotalFiles is 0 → falls through to no-context path
				TransferCtx: download.NewTransferContext(0, 0, download.TransferLifecycleDownloading),
			},
			// No context path because TotalFiles == 0, status is DOWNLOADING
			wantPercentDone:   0.25,
			wantStatus:        trStatusDownload,
			wantLeftUntilDone: 500,
		},
		{
			name: "with context, putio 100% + local 0%",
			input: progressInput{
				PutioPercentDone: 100,
				PutioStatus:      "COMPLETED",
				PutioSize:        5000,
				TransferCtx:      newTestTransferCtx(download.TransferLifecycleDownloading, 10, 0, 5000, 0),
			},
			wantPercentDone:   0.5, // 0.5 (putio) + 0.0 (local)
			wantStatus:        trStatusDownload,
			wantLeftUntilDone: 5000, // 0 putio + 5000 local
		},
		{
			name: "no context, IN_QUEUE status",
			input: progressInput{
				PutioPercentDone: 0,
				PutioStatus:      "IN_QUEUE",
				PutioSize:        2000,
			},
			wantPercentDone:   0.0,
			wantStatus:        trStatusDownloadWaiting,
			wantLeftUntilDone: 2000,
		},
		{
			name: "no context, ERROR status",
			input: progressInput{
				PutioPercentDone: 30,
				PutioStatus:      "ERROR",
				PutioSize:        1000,
			},
			wantPercentDone:   0.15,
			wantStatus:        trStatusStopped,
			wantLeftUntilDone: 700,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := calculateProgress(tt.input)

			const epsilon = 1e-9
			if diff := got.PercentDone - tt.wantPercentDone; diff > epsilon || diff < -epsilon {
				t.Errorf("PercentDone = %f, want %f", got.PercentDone, tt.wantPercentDone)
			}
			if got.Status != tt.wantStatus {
				t.Errorf("Status = %d, want %d", got.Status, tt.wantStatus)
			}
			if got.LeftUntilDone != tt.wantLeftUntilDone {
				t.Errorf("LeftUntilDone = %d, want %d", got.LeftUntilDone, tt.wantLeftUntilDone)
			}
		})
	}
}
