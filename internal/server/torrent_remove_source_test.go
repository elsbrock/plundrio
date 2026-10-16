package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/config"
	"github.com/elsbrock/plundrio/internal/download"
)

type deleteFileClient struct {
	*torrentAddClient
	deleteFileErr error
}

func (c *deleteFileClient) DeleteFile(ctx context.Context, fileID int64) error {
	_ = c.torrentAddClient.DeleteFile(ctx, fileID)
	return c.deleteFileErr
}

func removeSourceTransfer(t *testing.T, keepSeeding bool, deleteFileErr error) (*Server, *deleteFileClient, *download.Manager, error) {
	t.Helper()
	cfg := &config.Config{TargetDir: t.TempDir(), KeepSeeding: keepSeeding}
	manager := download.New(cfg, nil)
	transfer := &putio.Transfer{ID: 101, Name: "Book", FileID: 501, Status: "DOWNLOADING"}
	client := &deleteFileClient{
		torrentAddClient: &torrentAddClient{transfers: []*putio.Transfer{transfer}},
		deleteFileErr:    deleteFileErr,
	}
	srv := &Server{cfg: cfg, client: client, dlService: manager}
	_, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[101]}`))
	return srv, client, manager, err
}

// With keep-seeding and an incomplete local copy, torrent-remove is the only
// place that deletes the source file. If that fails, the transfer record must
// stay for a retry of torrent-remove. Without the record, the file stays on
// Put.io and uses quota.
func TestTorrentRemoveKeepsTransferWhenSourceDeletionFails(t *testing.T) {
	deleteErr := errors.New("put.io unavailable")
	_, client, manager, err := removeSourceTransfer(t, true, deleteErr)

	if !errors.Is(err, deleteErr) {
		t.Fatalf("error = %v, want the source deletion failure", err)
	}
	if len(client.deleted) != 0 {
		t.Fatalf("transfer record was deleted %v although the source file is still on Put.io", client.deleted)
	}
	if !manager.RemovalPending(101) {
		t.Fatal("removal marker was dropped, so processing is not suppressed until the retry")
	}
}

// Without keep-seeding the cleanup hook already deleted the source, so a failed
// DeleteFile here must not block removal. This test keeps the default behavior
// as it was: plundrio logs the error and deletes the transfer record.
func TestTorrentRemoveIgnoresSourceDeletionFailureByDefault(t *testing.T) {
	_, client, manager, err := removeSourceTransfer(t, false, errors.New("put.io unavailable"))

	if err != nil {
		t.Fatalf("default removal must not fail on a source deletion error, got %v", err)
	}
	if len(client.deleted) != 1 || client.deleted[0] != 101 {
		t.Fatalf("transfer record deletions = %v, want [101]", client.deleted)
	}
	if manager.RemovalPending(101) {
		t.Fatal("removal marker was left behind after a successful removal")
	}
}

// A source that is already gone is the goal of the removal, not a failure.
func TestTorrentRemoveTreatsMissingSourceAsDeleted(t *testing.T) {
	notFound := fmt.Errorf("delete file: %w", &putio.ErrorResponse{Type: "NotFound"})
	_, client, manager, err := removeSourceTransfer(t, true, notFound)

	if err != nil {
		t.Fatalf("missing source must not fail removal, got %v", err)
	}
	if len(client.deleted) != 1 || client.deleted[0] != 101 {
		t.Fatalf("transfer record deletions = %v, want [101]", client.deleted)
	}
	if manager.RemovalPending(101) {
		t.Fatal("removal marker was left behind after a successful removal")
	}
}

// A retry after a failed source deletion must finish the removal.
func TestTorrentRemoveRetryCompletesAfterSourceDeletionFailure(t *testing.T) {
	srv, client, manager, err := removeSourceTransfer(t, true, errors.New("put.io unavailable"))
	if err == nil {
		t.Fatal("first attempt must fail")
	}
	client.deleteFileErr = nil
	if _, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[101]}`)); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if len(client.deleted) != 1 || manager.RemovalPending(101) {
		t.Fatalf("retry did not finish: deleted=%v pending=%v", client.deleted, manager.RemovalPending(101))
	}
}

// With keep-seeding, the *arr app removes a torrent only while torrent-get
// reports it as removable. If the source deletion fails, the torrent must stay
// removable, so that the *arr app tries the removal again on its next refresh.
func TestTorrentRemoveSourceFailureKeepsTorrentRemovable(t *testing.T) {
	transferCtx := download.NewTransferContext(101, 1, download.TransferLifecycleProcessed)
	transferCtx.SetTotalSize(1000)
	transferCtx.AddDownloadedBytes(1000)
	transfer := &putio.Transfer{ID: 101, Hash: "kept", Name: "Book", FileID: 501, Status: "COMPLETED", PercentDone: 100, Size: 1000}
	service := &torrentAddDownloadService{
		transfers: []*putio.Transfer{transfer},
		contexts:  map[int64]*download.TransferContext{101: transferCtx},
	}
	deleteErr := errors.New("put.io unavailable")
	client := &deleteFileClient{
		torrentAddClient: &torrentAddClient{transfers: []*putio.Transfer{transfer}},
		deleteFileErr:    deleteErr,
	}
	srv := &Server{cfg: &config.Config{TargetDir: t.TempDir(), KeepSeeding: true}, client: client, dlService: service}

	_, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[101]}`))
	if !errors.Is(err, deleteErr) {
		t.Fatalf("error = %v, want the source deletion failure", err)
	}
	if len(client.deleted) != 0 {
		t.Fatalf("transfer record was deleted %v although the source file is still on Put.io", client.deleted)
	}
	if service.RemovalPending(101) {
		t.Fatal("removal marker was written, so torrent-get hides the torrent as stopped")
	}
	got := singleTorrentGet(t, srv)
	if got.Status != trStatusSeed || got.SeedIdleMode != transmissionLimitModeSingle || got.SecondsSeeding != 1 {
		t.Fatalf("torrent-get after the failure: status=%d seedIdleMode=%d secondsSeeding=%d, want a removable seeding torrent",
			got.Status, got.SeedIdleMode, got.SecondsSeeding)
	}

	client.deleteFileErr = nil
	if _, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[101]}`)); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if want := []int64{101}; !reflect.DeepEqual(client.deleted, want) {
		t.Fatalf("transfer record deletions = %v, want %v", client.deleted, want)
	}
	if want := []int64{501, 501}; !reflect.DeepEqual(client.deletedFiles, want) {
		t.Fatalf("source deletions = %v, want one per attempt %v", client.deletedFiles, want)
	}
}

type orderedRemovalClient struct {
	*torrentAddClient
	events []string
}

func (c *orderedRemovalClient) DeleteFile(ctx context.Context, fileID int64) error {
	c.events = append(c.events, fmt.Sprintf("file:%d", fileID))
	return c.torrentAddClient.DeleteFile(ctx, fileID)
}

func (c *orderedRemovalClient) DeleteTransfer(ctx context.Context, id int64) error {
	c.events = append(c.events, fmt.Sprintf("transfer:%d", id))
	return c.torrentAddClient.DeleteTransfer(ctx, id)
}

// With keep-seeding the source file is still on Put.io when the *arr app removes
// the torrent. Removal must delete that file before it deletes the transfer record.
func TestTorrentRemoveDeletesKeptSourceBeforeTransfer(t *testing.T) {
	cfg := &config.Config{TargetDir: t.TempDir(), KeepSeeding: true}
	manager := download.New(cfg, nil)
	transfer := &putio.Transfer{ID: 101, Name: "Book", FileID: 501, Status: "DOWNLOADING"}
	client := &orderedRemovalClient{torrentAddClient: &torrentAddClient{transfers: []*putio.Transfer{transfer}}}
	srv := &Server{cfg: cfg, client: client, dlService: manager}

	if _, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[101]}`)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"file:501", "transfer:101"}; !reflect.DeepEqual(client.events, want) {
		t.Fatalf("remote calls = %v, want %v", client.events, want)
	}
}
