package download

import (
	"os"
	"testing"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/config"
)

// seedingSourceID is the Put.io file ID of the source in these tests.
const seedingSourceID = 501

// completeTransferForCleanup drives a transfer with a Put.io source file
// through the coordinator so the registered cleanup hooks run.
func completeTransferForCleanup(t *testing.T, keepSeeding bool, deletedFiles chan int64) *Manager {
	t.Helper()
	m := New(&config.Config{
		TargetDir:   t.TempDir(),
		FolderID:    testFolderID,
		WorkerCount: 1,
		KeepSeeding: keepSeeding,
	}, &fakeClient{deletedFiles: deletedFiles})
	m.coordinator.InitiateTransfer(101, "completed", seedingSourceID, 0)
	if err := m.coordinator.StartDownload(101); err != nil {
		t.Fatal(err)
	}

	m.cleanupTransfer(101)

	ctx, ok := m.coordinator.GetTransferContext(101)
	if !ok || ctx.GetState() != TransferLifecycleProcessed {
		t.Fatalf("transfer was not marked processed: context=%v exists=%v", ctx, ok)
	}
	return m
}

func TestCleanupDeletesSourceFileByDefault(t *testing.T) {
	deletedFiles := make(chan int64, 1)
	completeTransferForCleanup(t, false, deletedFiles)

	select {
	case fileID := <-deletedFiles:
		if fileID != seedingSourceID {
			t.Fatalf("DeleteFile called with %d, want %d", fileID, seedingSourceID)
		}
	default:
		t.Fatal("DeleteFile was not called, but the default must delete the source after the local copy")
	}
}

func TestCleanupKeepsSourceFileWhenKeepSeeding(t *testing.T) {
	deletedFiles := make(chan int64, 1)
	completeTransferForCleanup(t, true, deletedFiles)

	select {
	case fileID := <-deletedFiles:
		t.Fatalf("DeleteFile called with %d, want the source kept on Put.io for seeding", fileID)
	default:
	}
}

// seedingClient lists one source file that has no local copy. This is the case
// when an *arr app moved the imported file away from the download directory.
func seedingClient() *fakeClient {
	return &fakeClient{files: func(int64) ([]*putio.File, error) {
		return []*putio.File{{ID: 900, Name: "movie.mkv", Size: 10}}, nil
	}}
}

func seedingTransfer() *putio.Transfer {
	return &putio.Transfer{ID: 101, Name: "Book", FileID: seedingSourceID, Status: "SEEDING"}
}

// seedingManager builds a manager whose state lives in dir, so that a second
// manager on the same dir behaves like a restarted process.
func seedingManager(dir string, keepSeeding bool) *Manager {
	return New(&config.Config{
		TargetDir:   dir,
		FolderID:    testFolderID,
		WorkerCount: 1,
		KeepSeeding: keepSeeding,
	}, seedingClient())
}

// finishLocalDownload drives a transfer through the first run. The test writes
// the manifest, then completes the download locally and runs the cleanup.
func finishLocalDownload(t *testing.T, m *Manager) {
	t.Helper()
	if _, ctx := m.processor.prepareTransfer(seedingTransfer()); ctx == nil {
		t.Fatal("prepareTransfer did not track the transfer")
	}
	if err := m.coordinator.FileCompleted(101); err != nil {
		t.Fatal(err)
	}
	m.cleanupTransfer(101)
	ctx, ok := m.coordinator.GetTransferContext(101)
	if !ok || ctx.GetState() != TransferLifecycleProcessed {
		t.Fatalf("transfer was not marked processed: context=%v exists=%v", ctx, ok)
	}
}

// After a restart, a kept source is still on Put.io but the local copy can be
// gone. The transfer must come back as Processed, not be downloaded again.
func TestKeepSeedingRestartDoesNotRedownloadProcessedTransfer(t *testing.T) {
	dir := t.TempDir()
	finishLocalDownload(t, seedingManager(dir, true))

	restarted := seedingManager(dir, true)
	restarted.processor.processTransfer(seedingTransfer())

	ctx, ok := restarted.GetTransferContext(101)
	if !ok || ctx.GetState() != TransferLifecycleProcessed {
		t.Fatalf("restarted transfer must be Processed, got %+v", ctx)
	}
	if queued := len(restarted.jobs); queued != 0 {
		t.Fatalf("restart queued %d download jobs for a transfer that already finished", queued)
	}
}

// SeedingKept does not look at keep-seeding. If someone turns the option off,
// a marked transfer comes back as Processed after the restart. The cleanup hook
// then deletes the source on Put.io, so nothing seeds forever and no download
// runs again.
func TestRestartWithoutKeepSeedingDeletesKeptSource(t *testing.T) {
	dir := t.TempDir()
	finishLocalDownload(t, seedingManager(dir, true))

	deletedFiles := make(chan int64, 1)
	client := seedingClient()
	client.deletedFiles = deletedFiles
	restarted := New(&config.Config{TargetDir: dir, FolderID: testFolderID, WorkerCount: 1}, client)
	restarted.processor.processTransfer(seedingTransfer())

	ctx, ok := restarted.GetTransferContext(101)
	if !ok || ctx.GetState() != TransferLifecycleProcessed {
		t.Fatalf("restarted transfer must be Processed, got %+v", ctx)
	}
	if queued := len(restarted.jobs); queued != 0 {
		t.Fatalf("restart queued %d download jobs for a transfer that already finished", queued)
	}
	select {
	case fileID := <-deletedFiles:
		if fileID != seedingSourceID {
			t.Fatalf("DeleteFile called with %d, want %d", fileID, seedingSourceID)
		}
	default:
		t.Fatal("DeleteFile was not called, but the kept source must go when keep-seeding is off")
	}
}

// If the cleanup hook cannot write the seeding marker, a restart downloads the
// transfer again. The next poll must write the missing marker.
func TestKeepSeedingPollRetriesFailedMarkerWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	transfer := seedingTransfer()
	transfer.SaveParentID = testFolderID
	client := seedingClient()
	client.transfers = func() ([]*putio.Transfer, error) { return []*putio.Transfer{transfer}, nil }
	m := New(&config.Config{TargetDir: dir, FolderID: testFolderID, WorkerCount: 1, KeepSeeding: true}, client)
	if _, ctx := m.processor.prepareTransfer(seedingTransfer()); ctx == nil {
		t.Fatal("prepareTransfer did not track the transfer")
	}
	if err := m.coordinator.FileCompleted(101); err != nil {
		t.Fatal(err)
	}

	stateDir := m.transferFiles.stateDir
	if err := os.Chmod(stateDir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0700) })
	m.cleanupTransfer(101)
	if m.SeedingKept(101) {
		t.Fatal("test setup: the seeding marker write did not fail")
	}

	if err := os.Chmod(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	m.processor.checkTransfers()
	m.processorWg.Wait()
	if !m.SeedingKept(101) {
		t.Fatal("poll did not write the missing seeding marker")
	}

	restarted := seedingManager(dir, true)
	restarted.processor.processTransfer(seedingTransfer())
	if queued := len(restarted.jobs); queued != 0 {
		t.Fatalf("restart queued %d download jobs for a transfer that already finished", queued)
	}
}

// Guard: a manifest alone only proves the download started. Plundrio must still
// resume an interrupted download after a restart.
func TestKeepSeedingRestartResumesInterruptedDownload(t *testing.T) {
	dir := t.TempDir()
	first := seedingManager(dir, true)
	if _, ctx := first.processor.prepareTransfer(seedingTransfer()); ctx == nil {
		t.Fatal("prepareTransfer did not track the transfer")
	}

	restarted := seedingManager(dir, true)
	restarted.processor.processTransfer(seedingTransfer())

	if queued := len(restarted.jobs); queued != 1 {
		t.Fatalf("interrupted download must be queued again, got %d jobs", queued)
	}
}

// The seeding marker is bookkeeping for one transfer, so torrent-remove must
// drop it together with the manifest.
func TestKeepSeedingMarkerIsRemovedWithTransfer(t *testing.T) {
	dir := t.TempDir()
	m := seedingManager(dir, true)
	finishLocalDownload(t, m)

	if entries, _ := os.ReadDir(m.transferFiles.stateDir); len(entries) != 2 {
		t.Fatalf("want a manifest and a seeding marker, got %d entries", len(entries))
	}
	m.RemoveTransfer(101)
	if entries, _ := os.ReadDir(m.transferFiles.stateDir); len(entries) != 0 {
		t.Fatalf("RemoveTransfer left %d state files behind", len(entries))
	}
}

// Without keep-seeding, the cleanup hook deletes the source, so plundrio needs
// no seeding marker.
func TestDefaultCleanupWritesNoSeedingMarker(t *testing.T) {
	m := seedingManager(t.TempDir(), false)
	finishLocalDownload(t, m)

	if entries, _ := os.ReadDir(m.transferFiles.stateDir); len(entries) != 1 {
		t.Fatalf("want only the manifest, got %d entries", len(entries))
	}
}

// After local completion, Put.io still lists the kept source as SEEDING on
// every poll. The monitor must leave the Processed transfer alone.
func TestKeepSeedingPollsDoNotReprocessProcessedTransfer(t *testing.T) {
	transfer := seedingTransfer()
	transfer.SaveParentID = testFolderID
	listings := 0
	client := &fakeClient{
		transfers: func() ([]*putio.Transfer, error) { return []*putio.Transfer{transfer}, nil },
		files: func(int64) ([]*putio.File, error) {
			listings++
			return []*putio.File{{ID: 900, Name: "movie.mkv", Size: 10}}, nil
		},
	}
	m := New(&config.Config{TargetDir: t.TempDir(), FolderID: testFolderID, WorkerCount: 1, KeepSeeding: true}, client)
	finishLocalDownload(t, m)
	listingsAfterFirstRun := listings

	for range 3 {
		m.processor.checkTransfers()
		m.processorWg.Wait()
	}

	ctx, ok := m.GetTransferContext(101)
	if !ok || ctx.GetState() != TransferLifecycleProcessed {
		t.Fatalf("polls changed the Processed transfer: %+v", ctx)
	}
	if listings != listingsAfterFirstRun {
		t.Fatalf("polls listed the kept source %d more times", listings-listingsAfterFirstRun)
	}
	if queued := len(m.jobs); queued != 0 {
		t.Fatalf("polls queued %d download jobs", queued)
	}
}
