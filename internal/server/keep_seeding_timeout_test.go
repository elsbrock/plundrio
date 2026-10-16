package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/config"
	"github.com/elsbrock/plundrio/internal/download"
)

// timeoutTestNow is the fixed time of the clock in the keep-seeding-timeout tests.
var timeoutTestNow = time.Date(2026, time.October, 17, 20, 0, 0, 0, time.UTC)

// serverWithProcessedTransfer builds a server with one locally processed
// transfer and a fixed clock.
func serverWithProcessedTransfer(t *testing.T, cfg *config.Config, transfer *putio.Transfer) *Server {
	t.Helper()
	return serverWithTransfer(t, cfg, transfer, download.TransferLifecycleProcessed)
}

// serverWithTransfer builds a server with one transfer in the given state, all
// local bytes downloaded, and a fixed clock.
func serverWithTransfer(t *testing.T, cfg *config.Config, transfer *putio.Transfer, state download.TransferLifecycleState) *Server {
	t.Helper()
	ctx := download.NewTransferContext(transfer.ID, 1, state)
	ctx.SetTotalSize(1000)
	ctx.AddDownloadedBytes(1000)
	cfg.TargetDir = t.TempDir()
	return &Server{
		cfg: cfg,
		dlService: &torrentAddDownloadService{
			transfers: []*putio.Transfer{transfer},
			contexts:  map[int64]*download.TransferContext{transfer.ID: ctx},
		},
		now: func() time.Time { return timeoutTestNow },
	}
}

// finishedAgo returns the finish time of a transfer that ended `age` earlier
// than the fixed clock.
func finishedAgo(age time.Duration) *putio.Time {
	return &putio.Time{Time: timeoutTestNow.Add(-age)}
}

func TestTorrentGetKeepSeedingTimeout(t *testing.T) {
	tests := []struct {
		name         string
		keepSeeding  bool
		timeout      time.Duration
		putioStatus  string
		finishedAt   *putio.Time
		wantIdleMode int
	}{
		{name: "older than the limit", keepSeeding: true, timeout: 72 * time.Hour, putioStatus: "SEEDING", finishedAt: finishedAgo(73 * time.Hour), wantIdleMode: transmissionLimitModeSingle},
		{name: "exactly at the limit", keepSeeding: true, timeout: 72 * time.Hour, putioStatus: "SEEDING", finishedAt: finishedAgo(72 * time.Hour), wantIdleMode: transmissionLimitModeSingle},
		{name: "one minute before the limit", keepSeeding: true, timeout: 72 * time.Hour, putioStatus: "SEEDING", finishedAt: finishedAgo(72*time.Hour - time.Minute), wantIdleMode: transmissionLimitModeUnlimited},
		{name: "limit disabled", keepSeeding: true, timeout: 0, putioStatus: "SEEDING", finishedAt: finishedAgo(1000 * time.Hour), wantIdleMode: transmissionLimitModeUnlimited},
		{name: "no finish time", keepSeeding: true, timeout: 72 * time.Hour, putioStatus: "SEEDING", finishedAt: nil, wantIdleMode: transmissionLimitModeUnlimited},
		{name: "zero finish time", keepSeeding: true, timeout: 72 * time.Hour, putioStatus: "SEEDING", finishedAt: &putio.Time{}, wantIdleMode: transmissionLimitModeUnlimited},
		// A COMPLETED transfer is removable without the limit.
		{name: "completed before the limit", keepSeeding: true, timeout: 72 * time.Hour, putioStatus: "COMPLETED", finishedAt: finishedAgo(time.Hour), wantIdleMode: transmissionLimitModeSingle},
		// Without keep-seeding the limit has no effect and the default applies:
		// a processed transfer is removable.
		{name: "keep-seeding off", keepSeeding: false, timeout: 72 * time.Hour, putioStatus: "SEEDING", finishedAt: finishedAgo(time.Hour), wantIdleMode: transmissionLimitModeSingle},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := serverWithProcessedTransfer(t,
				&config.Config{KeepSeeding: tt.keepSeeding, KeepSeedingTimeout: tt.timeout},
				&putio.Transfer{ID: 1, Hash: "processed", Status: tt.putioStatus, PercentDone: 100, Size: 1000, FinishedAt: tt.finishedAt})

			got := singleTorrentGet(t, server)

			if got.Status != trStatusSeed {
				t.Errorf("status = %d, want %d (seed)", got.Status, trStatusSeed)
			}
			if got.SeedIdleMode != tt.wantIdleMode {
				t.Errorf("seedIdleMode = %d, want %d", got.SeedIdleMode, tt.wantIdleMode)
			}
		})
	}
}

// seedingTimedOut is a plain yes/no check. It must leave no log record.
func TestSeedingTimedOutHasNoSideEffect(t *testing.T) {
	server := serverWithProcessedTransfer(t,
		&config.Config{KeepSeeding: true, KeepSeedingTimeout: time.Hour},
		&putio.Transfer{ID: 7, Status: "SEEDING"})
	young := &putio.Transfer{ID: 7, Status: "SEEDING", FinishedAt: finishedAgo(time.Minute)}
	old := &putio.Transfer{ID: 7, Status: "SEEDING", FinishedAt: finishedAgo(2 * time.Hour)}

	if server.seedingTimedOut(young) {
		t.Fatal("a young transfer must not time out")
	}
	if !server.seedingTimedOut(old) {
		t.Fatal("an old transfer must time out")
	}
	if _, logged := server.seedingTimeoutLogged.Load(int64(7)); logged {
		t.Fatal("seedingTimedOut must not write the log record")
	}
}

// torrent-get runs often, so the time limit must log once per transfer.
func TestLogSeedingTimeoutLogsOncePerTransfer(t *testing.T) {
	server := serverWithProcessedTransfer(t,
		&config.Config{KeepSeeding: true, KeepSeedingTimeout: time.Hour},
		&putio.Transfer{ID: 7, Status: "SEEDING"})
	young := &putio.Transfer{ID: 7, Status: "SEEDING", FinishedAt: finishedAgo(time.Minute)}
	old := &putio.Transfer{ID: 7, Status: "SEEDING", FinishedAt: finishedAgo(2 * time.Hour)}
	completed := &putio.Transfer{ID: 8, Status: "COMPLETED", FinishedAt: finishedAgo(2 * time.Hour)}

	server.logSeedingTimeout(young)
	if _, logged := server.seedingTimeoutLogged.Load(int64(7)); logged {
		t.Fatal("a young transfer must not be marked as logged")
	}
	server.logSeedingTimeout(completed)
	if _, logged := server.seedingTimeoutLogged.Load(int64(8)); logged {
		t.Fatal("a COMPLETED transfer did not need the limit, so it must not be logged")
	}
	for i := 0; i < 3; i++ {
		server.logSeedingTimeout(old)
	}
	if _, logged := server.seedingTimeoutLogged.Load(int64(7)); !logged {
		t.Fatal("an old transfer must be marked as logged")
	}
}

// A transfer can leave put.io without a torrent-remove. torrent-get must then
// drop its log record, or the record stays for the life of the process.
func TestTorrentGetForgetsSeedingTimeoutLogOfVanishedTransfer(t *testing.T) {
	transfer := &putio.Transfer{ID: 1, Hash: "processed", Name: "Book", FileID: 501, Status: "SEEDING", PercentDone: 100, Size: 1000, FinishedAt: finishedAgo(2 * time.Hour)}
	server := serverWithProcessedTransfer(t,
		&config.Config{KeepSeeding: true, KeepSeedingTimeout: time.Hour},
		transfer)

	if got := singleTorrentGet(t, server); got.SeedIdleMode != transmissionLimitModeSingle {
		t.Fatalf("seedIdleMode = %d, want %d: the transfer must time out first", got.SeedIdleMode, transmissionLimitModeSingle)
	}
	if _, logged := server.seedingTimeoutLogged.Load(transfer.ID); !logged {
		t.Fatal("the time limit was not logged, so the test proves nothing")
	}

	server.dlService.(*torrentAddDownloadService).transfers = nil
	if _, err := server.handleTorrentGet(context.Background(), json.RawMessage(`{"fields":["id"]}`)); err != nil {
		t.Fatalf("torrent-get failed: %v", err)
	}
	if _, logged := server.seedingTimeoutLogged.Load(transfer.ID); logged {
		t.Fatal("torrent-get kept the log record of a transfer that left put.io")
	}
}

// The limit applies only to a transfer that plundrio processed. A transfer
// that plundrio did not process yet reports as seeding with no bytes left. Only
// its state keeps it out of removal.
func TestTorrentGetKeepSeedingTimeoutIgnoresUnprocessedTransfer(t *testing.T) {
	server := serverWithTransfer(t,
		&config.Config{KeepSeeding: true, KeepSeedingTimeout: time.Hour},
		&putio.Transfer{ID: 1, Hash: "completed", Status: "SEEDING", PercentDone: 100, Size: 1000, FinishedAt: finishedAgo(2 * time.Hour)},
		download.TransferLifecycleCompleted)

	got := singleTorrentGet(t, server)

	if got.Status != trStatusSeed {
		t.Fatalf("status = %d, want %d (seed)", got.Status, trStatusSeed)
	}
	if got.SeedIdleMode != transmissionLimitModeUnlimited {
		t.Fatalf("seedIdleMode = %d, want %d: a transfer that is not processed must not be removable", got.SeedIdleMode, transmissionLimitModeUnlimited)
	}
}

// torrent-remove drops all tracking for a transfer. It must also delete the log
// record of the time limit. Otherwise the record stays for the life of the
// process.
func TestTorrentRemoveForgetsSeedingTimeoutLog(t *testing.T) {
	transfer := &putio.Transfer{ID: 1, Hash: "processed", Name: "Book", FileID: 501, Status: "SEEDING", PercentDone: 100, Size: 1000, FinishedAt: finishedAgo(2 * time.Hour)}
	server := serverWithProcessedTransfer(t,
		&config.Config{KeepSeeding: true, KeepSeedingTimeout: time.Hour},
		transfer)
	server.client = &torrentAddClient{transfers: []*putio.Transfer{transfer}}

	if got := singleTorrentGet(t, server); got.SeedIdleMode != transmissionLimitModeSingle {
		t.Fatalf("seedIdleMode = %d, want %d: the transfer must time out before the removal", got.SeedIdleMode, transmissionLimitModeSingle)
	}
	if _, logged := server.seedingTimeoutLogged.Load(transfer.ID); !logged {
		t.Fatal("the time limit was not logged, so the test proves nothing")
	}

	if _, err := server.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[1]}`)); err != nil {
		t.Fatalf("torrent-remove failed: %v", err)
	}
	if _, logged := server.seedingTimeoutLogged.Load(transfer.ID); logged {
		t.Fatal("torrent-remove kept the log record of the time limit")
	}
}
