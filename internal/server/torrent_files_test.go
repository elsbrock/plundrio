package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/config"
	"github.com/elsbrock/plundrio/internal/download"
)

func TestHandleTorrentGetSelectors(t *testing.T) {
	transfers := []*putio.Transfer{
		{ID: 101, Hash: "ABC123", Name: "Book One"},
		{ID: 202, Hash: "DEF456", Name: "Book Two"},
	}
	tests := []struct {
		name    string
		args    string
		wantIDs []int64
		wantErr bool
	}{
		{"numeric array", `{"ids":[101],"fields":["id"]}`, []int64{101}, false},
		{"case-insensitive hash", `{"ids":["def456"],"fields":["id"]}`, []int64{202}, false},
		{"unknown", `{"ids":[999],"fields":["id"]}`, []int64{}, false},
		{"non-integral", `{"ids":[1.5],"fields":["id"]}`, nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{
				cfg:       &config.Config{TargetDir: t.TempDir()},
				client:    &torrentAddClient{transfers: transfers},
				dlService: &torrentAddDownloadService{transfers: transfers},
			}
			response, err := server.handleTorrentGet(context.Background(), json.RawMessage(tt.args))
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			var decoded struct {
				Torrents []struct {
					ID int64 `json:"id"`
				} `json:"torrents"`
			}
			decodeResponse(t, response, &decoded)
			got := make([]int64, len(decoded.Torrents))
			for i, torrent := range decoded.Torrents {
				got[i] = torrent.ID
			}
			if !reflect.DeepEqual(got, tt.wantIDs) {
				t.Fatalf("IDs = %v, want %v", got, tt.wantIDs)
			}
		})
	}
}

func TestHandleTorrentGetReturnsExactManifest(t *testing.T) {
	transfers := []*putio.Transfer{
		{ID: 101, Name: "Same Book", Status: "COMPLETED"},
		{ID: 202, Name: "Same Book", Status: "COMPLETED"},
	}
	service := &torrentAddDownloadService{
		transfers: transfers,
		files: map[int64][]download.TransferFile{
			101: {
				{Name: "Same Book/one.m4b", Length: 3},
				{Name: "Same Book/disc-2/two.m4b", Length: 6},
			},
			202: {{Name: "Same Book/unrelated.m4b", Length: 10}},
		},
	}
	server := &Server{cfg: &config.Config{TargetDir: t.TempDir()}, dlService: service}

	response, err := server.handleTorrentGet(context.Background(), json.RawMessage(`{"ids":[101],"fields":["id","files"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Torrents []struct {
			ID    int64              `json:"id"`
			Files []transmissionFile `json:"files"`
		} `json:"torrents"`
	}
	decodeResponse(t, response, &decoded)
	want := []transmissionFile{
		{Length: 3, Name: "Same Book/one.m4b"},
		{Length: 6, Name: "Same Book/disc-2/two.m4b"},
	}
	if len(decoded.Torrents) != 1 || decoded.Torrents[0].ID != 101 || !reflect.DeepEqual(decoded.Torrents[0].Files, want) {
		t.Fatalf("torrents = %+v, want transfer 101 files %+v", decoded.Torrents, want)
	}
}

func TestHandleTorrentGetIncludesTransfersWithoutManifest(t *testing.T) {
	transfers := []*putio.Transfer{
		{ID: 101, Name: "Downloading", Status: "COMPLETED"},
		{ID: 202, Name: "Ready", Status: "COMPLETED"},
	}
	service := &torrentAddDownloadService{
		transfers: transfers,
		files: map[int64][]download.TransferFile{
			202: {{Name: "Ready/book.m4b", Length: 10}},
		},
	}
	server := &Server{cfg: &config.Config{TargetDir: t.TempDir()}, dlService: service}

	response, err := server.handleTorrentGet(context.Background(), json.RawMessage(`{"fields":["id","files"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Torrents []struct {
			ID    int64              `json:"id"`
			Files []transmissionFile `json:"files"`
		} `json:"torrents"`
	}
	decodeResponse(t, response, &decoded)
	if len(decoded.Torrents) != 2 {
		t.Fatalf("torrents = %+v, want both transfers", decoded.Torrents)
	}
	if decoded.Torrents[0].ID != 101 || decoded.Torrents[0].Files == nil || len(decoded.Torrents[0].Files) != 0 {
		t.Fatalf("pre-manifest transfer = %+v, want transfer 101 with an empty file list", decoded.Torrents[0])
	}
	if decoded.Torrents[1].ID != 202 || len(decoded.Torrents[1].Files) != 1 {
		t.Fatalf("manifest-backed transfer = %+v, want transfer 202 with one file", decoded.Torrents[1])
	}
}

func TestHandleTorrentGetRejectsUnsafeManifest(t *testing.T) {
	transfer := &putio.Transfer{ID: 101, Name: "Book"}
	service := &torrentAddDownloadService{
		transfers: []*putio.Transfer{transfer},
		files: map[int64][]download.TransferFile{
			101: {{Name: "../outside.m4b", Length: 1}},
		},
	}
	server := &Server{cfg: &config.Config{TargetDir: t.TempDir()}, dlService: service}
	if _, err := server.handleTorrentGet(context.Background(), json.RawMessage(`{"ids":[101],"fields":["files"]}`)); err == nil {
		t.Fatal("expected unsafe manifest to fail")
	}
}

func TestHandleTorrentRemoveNumericID(t *testing.T) {
	for _, tt := range []struct {
		name             string
		fileID           int64
		wantDeletedFiles []int64
	}{
		{"source present", 501, []int64{501}},
		{"source already cleaned", 0, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			transfer := &putio.Transfer{ID: 101, Name: "Book", FileID: tt.fileID}
			client := &torrentAddClient{transfers: []*putio.Transfer{transfer}}
			service := &torrentAddDownloadService{transfers: []*putio.Transfer{transfer}}
			server := &Server{cfg: &config.Config{TargetDir: t.TempDir()}, client: client, dlService: service}

			if _, err := server.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[101]}`)); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(client.deletedFiles, tt.wantDeletedFiles) {
				t.Fatalf("deleted files = %v, want %v", client.deletedFiles, tt.wantDeletedFiles)
			}
			if !reflect.DeepEqual(client.deleted, []int64{101}) || !reflect.DeepEqual(service.removedTransfers, []int64{101}) {
				t.Fatalf("deleted transfers = %v, local removals = %v", client.deleted, service.removedTransfers)
			}
		})
	}
}

func TestHandleTorrentRemoveBoundsFailuresAndRetainsOwnership(t *testing.T) {
	transfer := &putio.Transfer{ID: 101, Name: "Book", FileID: 501}
	client := &torrentAddClient{
		transfers:         []*putio.Transfer{transfer},
		deleteTransferErr: errors.New("remote deletion failed"),
	}
	service := &torrentAddDownloadService{
		categories: map[int64]string{101: "books"},
		files: map[int64][]download.TransferFile{
			101: {{Name: "Book/book.m4b", Length: 10}},
		},
		transfers: []*putio.Transfer{transfer},
	}
	server := &Server{cfg: &config.Config{TargetDir: t.TempDir()}, client: client, dlService: service}

	if _, err := server.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[101]}`)); err == nil {
		t.Fatal("expected actionable remote deletion error")
	}
	if len(client.deleted) != 3 {
		t.Fatalf("remote attempts = %d, want 3", len(client.deleted))
	}

	if len(service.removedTransfers) != 0 {
		t.Fatalf("removed local transfers = %v, want none", service.removedTransfers)
	}
	if got := service.GetCategory(101); got != "books" {
		t.Fatalf("retained category = %q, want books", got)
	}
	if len(service.categories) != 0 || !service.RemovalPending(101) {
		t.Fatal("failed removal retained active category tracking")
	}
	if _, ok := service.files[101]; !ok {
		t.Fatal("durable manifest was discarded after remote deletion failed")
	}
	response, err := server.handleTorrentGet(context.Background(), json.RawMessage(`{"ids":[101]}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Torrents []struct {
			Status      int
			ErrorString string
		}
	}
	decodeResponse(t, response, &decoded)
	if len(decoded.Torrents) != 1 || decoded.Torrents[0].Status != trStatusStopped || decoded.Torrents[0].ErrorString == "" {
		t.Fatalf("pending removal not actionable: %+v", decoded)
	}
	client.deleteTransferErr = nil
	if _, err := server.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[101]}`)); err != nil {
		t.Fatal(err)
	}
	if service.RemovalPending(101) || len(service.files) != 0 {
		t.Fatal("successful retry retained removal state")
	}
}

func TestDeleteLocalDataRejectsManifestNamespace(t *testing.T) {
	targetDir := t.TempDir()
	manifestPath := filepath.Join(targetDir, ".plundrio-files", "101.json")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte("manifest"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := deleteLocalData(targetDir, ".plundrio-files"); err == nil {
		t.Fatal("expected reserved manifest namespace deletion to fail")
	}
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("manifest was not preserved: %v", err)
	}
}

func decodeResponse(t *testing.T, response interface{}, target interface{}) {
	t.Helper()
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}

type pruningRemovalClient struct {
	*torrentAddClient
	manager *download.Manager
}

func (c *pruningRemovalClient) DeleteTransfer(ctx context.Context, id int64) error {
	if err := c.torrentAddClient.DeleteTransfer(ctx, id); err != nil {
		return err
	}
	// Simulate the monitor observing remote absence after deletion, before
	// the RPC request resumes its local cleanup. This is the same reclamation
	// operation invoked by pruneRemovals when no workers remain active.
	c.manager.RemoveTransfer(id)
	return nil
}

func TestTorrentRemoveKeepsCategoryWhenMonitorPrunesMarker(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"Book/keep", "books/Book/remove"} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("payload"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{TargetDir: root, UseCategoriesTarget: true}
	manager := download.New(cfg, nil)
	manager.SetCategory(101, "books")
	client := &pruningRemovalClient{
		torrentAddClient: &torrentAddClient{transfers: []*putio.Transfer{{ID: 101, Name: "Book"}}},
		manager:          manager,
	}
	srv := &Server{cfg: cfg, client: client, dlService: manager}
	if _, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[101],"delete-local-data":true}`)); err != nil {
		t.Fatal(err)
	}
	if manager.RemovalPending(101) {
		t.Fatal("test did not reclaim removal marker")
	}
	if _, err := os.Stat(filepath.Join(root, "Book", "keep")); err != nil {
		t.Fatalf("unrelated bare-path payload was deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "books", "Book")); !os.IsNotExist(err) {
		t.Fatalf("selected category payload was not removed: %v", err)
	}
}
