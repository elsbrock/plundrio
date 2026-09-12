package download

import (
	"strings"
	"testing"
	"time"

	"github.com/elsbrock/go-putio"
)

type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
}

func TestStallTrackerProgressTimeoutAndRecovery(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)}
	tracker := newStallTracker(2*time.Hour, clock.Now)
	transfer := &putio.Transfer{ID: 42, Status: "DOWNLOADING", Downloaded: 100}

	tracker.Observe([]*putio.Transfer{transfer})
	clock.Advance(90 * time.Minute)
	tracker.Observe([]*putio.Transfer{transfer})
	if got := tracker.Error(transfer.ID); got != "" {
		t.Fatalf("active transfer reported stalled before timeout: %q", got)
	}

	transfer.Downloaded = 200
	tracker.Observe([]*putio.Transfer{transfer})
	clock.Advance(2 * time.Hour)
	tracker.Observe([]*putio.Transfer{transfer})
	if got := tracker.Error(transfer.ID); !strings.Contains(got, "no byte progress for 2h0m0s") {
		t.Fatalf("stalled transfer error = %q, want timeout detail", got)
	}

	transfer.Downloaded = 201
	tracker.Observe([]*putio.Transfer{transfer})
	if got := tracker.Error(transfer.ID); got != "" {
		t.Fatalf("progress did not immediately clear stalled error: %q", got)
	}
}

func TestStallTrackerPreservesBaselineAcrossCompleting(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	tracker := newStallTracker(time.Hour, clock.Now)
	transfer := &putio.Transfer{ID: 42, Status: "DOWNLOADING", Downloaded: 100}
	tracker.Observe([]*putio.Transfer{transfer})
	clock.Advance(50 * time.Minute)
	transfer.Status = "COMPLETING"
	tracker.Observe([]*putio.Transfer{transfer})
	clock.Advance(20 * time.Minute)
	if got := tracker.Error(42); got != "" {
		t.Fatalf("COMPLETING transfer reported as stalled: %q", got)
	}
	transfer.Status = "DOWNLOADING"
	tracker.Observe([]*putio.Transfer{transfer})
	if tracker.Error(42) == "" {
		t.Fatal("transient COMPLETING status reset the no-progress baseline")
	}
	transfer.Status = "COMPLETING"
	transfer.Downloaded++
	tracker.Observe([]*putio.Transfer{transfer})
	transfer.Status = "DOWNLOADING"
	tracker.Observe([]*putio.Transfer{transfer})
	if tracker.Error(42) != "" {
		t.Fatal("byte progress during COMPLETING did not reset baseline")
	}
}

func TestStallTrackerRestartEstablishesFreshBaseline(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)}
	transfer := &putio.Transfer{ID: 42, Status: "DOWNLOADING", Downloaded: 100}

	beforeRestart := newStallTracker(time.Hour, clock.Now)
	beforeRestart.Observe([]*putio.Transfer{transfer})
	clock.Advance(time.Hour)
	beforeRestart.Observe([]*putio.Transfer{transfer})
	if got := beforeRestart.Error(transfer.ID); got == "" {
		t.Fatal("transfer was not stalled before restart")
	}

	afterRestart := newStallTracker(time.Hour, clock.Now)
	afterRestart.Observe([]*putio.Transfer{transfer})
	if got := afterRestart.Error(transfer.ID); got != "" {
		t.Fatalf("fresh tracker immediately reported transfer stalled: %q", got)
	}
}

func TestStallTrackerDisabledAndInactiveTransfers(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)}
	disabled := newStallTracker(0, clock.Now)
	transfer := &putio.Transfer{ID: 42, Status: "DOWNLOADING"}

	disabled.Observe([]*putio.Transfer{transfer})
	clock.Advance(24 * time.Hour)
	disabled.Observe([]*putio.Transfer{transfer})
	if got := disabled.Error(transfer.ID); got != "" {
		t.Fatalf("disabled tracker reported stalled transfer: %q", got)
	}

	enabled := newStallTracker(time.Hour, clock.Now)
	enabled.Observe([]*putio.Transfer{transfer})
	clock.Advance(time.Hour)
	enabled.Observe([]*putio.Transfer{transfer})
	if got := enabled.Error(transfer.ID); got == "" {
		t.Fatal("transfer was not stalled before leaving downloading state")
	}

	transfer.Status = "COMPLETED"
	enabled.Observe([]*putio.Transfer{transfer})
	if got := enabled.Error(transfer.ID); got != "" {
		t.Fatalf("inactive transfer retained stalled error: %q", got)
	}
}

func TestTransferProcessorPublishesStallState(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)}
	transfer := &putio.Transfer{
		ID:           42,
		Status:       "DOWNLOADING",
		Downloaded:   100,
		SaveParentID: testFolderID,
	}
	manager := newManagerForTest(t, &fakeClient{
		transfers: func() ([]*putio.Transfer, error) {
			transferCopy := *transfer
			return []*putio.Transfer{&transferCopy}, nil
		},
	})
	manager.processor.stallTracker = newStallTracker(time.Hour, clock.Now)

	manager.processor.checkTransfers()
	clock.Advance(time.Hour)
	manager.processor.checkTransfers()

	transfers := manager.GetTransfers()
	if got := len(transfers); got != 1 {
		t.Fatalf("published transfer count = %d, want 1", got)
	}
	if got := transfers[0].ErrorMessage; got == "" {
		t.Fatal("processor did not publish the detected stall through the manager")
	}
}
