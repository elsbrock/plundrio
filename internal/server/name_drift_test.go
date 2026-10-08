package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/config"
	"github.com/elsbrock/plundrio/internal/download"
)

// Supply the latest mock Put.io poll while using the real persisted file store.
type manifestRPCService struct {
	*download.Manager
	transfers []*putio.Transfer
	contexts  map[int64]*download.TransferContext
}

func (s *manifestRPCService) GetTransfers() []*putio.Transfer { return s.transfers }

func (s *manifestRPCService) GetTransferContext(id int64) (*download.TransferContext, bool) {
	ctx, ok := s.contexts[id]
	return ctx, ok
}

type manifestRPCTorrent struct {
	ID           int64
	Name         string
	HashString   string
	Error        int
	ErrorString  string
	Status       int
	SeedIdleMode int
	PercentDone  float64
	Files        []transmissionFile
}

func manifestRPC(t *testing.T, srv *Server, arguments string) []manifestRPCTorrent {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/transmission/rpc", strings.NewReader(
		`{"method":"torrent-get","tag":7,"arguments":`+arguments+`}`))
	req.Header.Set("X-Transmission-Session-Id", "123")
	response := httptest.NewRecorder()
	srv.handleRPC(response, req)
	var decoded struct {
		Result    string
		Tag       int
		Arguments struct{ Torrents []manifestRPCTorrent }
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || decoded.Result != "success" || decoded.Tag != 7 {
		t.Fatalf("files-inclusive RPC failed: HTTP %d %s", response.Code, response.Body.String())
	}
	return decoded.Arguments.Torrents
}

func writeManifestFixture(t *testing.T, root, relative string, data []byte) {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

// Run with: go test ./internal/server -run '^TestManifestNameDriftRPC$' -count=1 -v
func TestManifestNameDriftRPC(t *testing.T) {
	root := t.TempDir()
	const manifest = `[{"name":"old-root/file.epub","length":4}]`
	writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(manifest))
	writeManifestFixture(t, root, "old-root/file.epub", []byte("book"))
	before, err := os.Stat(filepath.Join(root, "old-root/file.epub"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{TargetDir: root}
	for _, name := range []string{"old-root", "new-root"} {
		t.Run(name, func(t *testing.T) {
			// A fresh manager exercises the on-disk legacy format, not cached state.
			service := &manifestRPCService{Manager: download.New(cfg, nil), transfers: []*putio.Transfer{
				{ID: 101, Hash: "ABC123", Name: name, Status: "COMPLETED", PercentDone: 100, Size: 4},
			}}
			srv := &Server{cfg: cfg, dlService: service}
			for _, selector := range []string{`101`, `"abc123"`} {
				t.Run(selector, func(t *testing.T) {
					// This masks the bug: fields without files do not validate ownership.
					ids := manifestRPC(t, srv, fmt.Sprintf(`{"ids":[%s],"fields":["id","hashString"]}`, selector))
					if len(ids) != 1 || ids[0].ID != 101 {
						t.Fatalf("ID-only control = %+v", ids)
					}
					got := manifestRPC(t, srv, fmt.Sprintf(`{"ids":[%s],"fields":["id","name","hashString","files","error","errorString"]}`, selector))
					want := []transmissionFile{{Name: "old-root/file.epub", Length: 4}}
					// The name stays the locally owned root so the importer's
					// downloadDir/name output path keeps resolving.
					if len(got) != 1 || got[0].ID != 101 || got[0].Name != "old-root" || got[0].HashString != "ABC123" || got[0].Error != 0 || !reflect.DeepEqual(got[0].Files, want) {
						t.Fatalf("files-inclusive response = %+v, want unchanged old-root files %+v", got, want)
					}
					// downloadDir/name must still contain every reported file.
					if !strings.HasPrefix(want[0].Name, got[0].Name+"/") {
						t.Fatalf("reported name %q does not contain %q", got[0].Name, want[0].Name)
					}
				})
			}
		})
	}
	after, err := os.Stat(filepath.Join(root, "old-root/file.epub"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("payload moved or replaced: %v", err)
	}
	payload, err := os.ReadFile(filepath.Join(root, "old-root/file.epub"))
	if err != nil || string(payload) != "book" || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("payload contents or metadata changed: %q, %v", payload, err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".plundrio-files/101.json"))
	if err != nil || string(data) != manifest {
		t.Fatalf("manifest changed: %q, %v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "new-root")); !os.IsNotExist(err) {
		t.Fatalf("remote name created a local root: %v", err)
	}
}

func TestManifestNameDriftStoredHashRPCAndRemoval(t *testing.T) {
	root := t.TempDir()
	const stored = `{"version":1,"transferId":101,"localRoot":"old-root","files":[{"name":"old-root/file.epub","length":4}]}`
	writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(stored))
	writeManifestFixture(t, root, "old-root/file.epub", []byte("book"))
	before := localSnapshot(t, root)
	cfg := &config.Config{TargetDir: root}
	transfers := []*putio.Transfer{
		{ID: 101, Hash: "DEF456", Name: "new-root", Status: "COMPLETED", PercentDone: 100},
		// Matching the historical hash never grants another ID ownership.
		{ID: 202, Hash: "ABC123", Name: "old-root", FileID: 502},
	}
	client := &torrentAddClient{transfers: transfers}
	manager := download.New(cfg, nil)
	srv := &Server{cfg: cfg, client: client, dlService: &manifestRPCService{Manager: manager, transfers: transfers}}
	for _, selector := range []string{`101`, `"def456"`} {
		got := manifestRPC(t, srv, fmt.Sprintf(`{"ids":[%s],"fields":["id","name","hashString","files","error","errorString"]}`, selector))
		want := []transmissionFile{{Name: "old-root/file.epub", Length: 4}}
		if len(got) != 1 || got[0].ID != 101 || got[0].HashString != "DEF456" || got[0].Name != "old-root" || got[0].Error != 0 || !reflect.DeepEqual(got[0].Files, want) {
			t.Fatalf("same-ID hash change rejected via %s: %+v", selector, got)
		}
	}
	for _, selector := range []string{`202`, `"abc123"`} {
		_, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(fmt.Sprintf(`{"ids":[%s],"delete-local-data":true}`, selector)))
		if err == nil || !strings.Contains(err.Error(), "collides with transfer 101") {
			t.Fatalf("another numeric ID could delete the owner's root via %s: %v", selector, err)
		}
	}
	if len(client.deleted) != 0 || len(client.deletedFiles) != 0 || manager.RemovalPending(202) {
		t.Fatal("refused removal mutated remote state or published a removal marker")
	}
	localUnchanged(t, root, before)
}

func TestManifestNameDriftMixedRPC(t *testing.T) {
	root := t.TempDir()
	writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(`[{"name":"old-root/file.epub","length":4}]`))
	writeManifestFixture(t, root, ".plundrio-files/202.json", []byte(`[{"name":"new-root/other.epub","length":5}]`))
	writeManifestFixture(t, root, ".plundrio-files/303.json", []byte(`[{"name":"missing-root/file.epub","length":4}]`))
	writeManifestFixture(t, root, "old-root/file.epub", []byte("book"))
	writeManifestFixture(t, root, "new-root/other.epub", []byte("other"))
	cfg := &config.Config{TargetDir: root}
	service := &manifestRPCService{Manager: download.New(cfg, nil), transfers: []*putio.Transfer{
		{ID: 101, Hash: "ABC123", Name: "new-root", Status: "COMPLETED", PercentDone: 100},
		// Equal remote names do not imply equal ownership: these roots are disjoint.
		{ID: 202, Hash: "DEF456", Name: "new-root", Status: "COMPLETED", PercentDone: 100},
		{ID: 303, Hash: "BAD789", Name: "bad-new", Status: "COMPLETED", PercentDone: 100},
	}}
	srv := &Server{cfg: cfg, dlService: service}
	for _, ids := range []string{``, `"ids":[101,"def456",303],`} {
		torrents := manifestRPC(t, srv, `{`+ids+`"fields":["id","name","files","error","errorString","status","seedIdleMode"]}`)
		if len(torrents) != 3 {
			t.Fatalf("lost unrelated transfers: %+v", torrents)
		}
		for i, want := range []string{"old-root/file.epub", "new-root/other.epub"} {
			if torrents[i].Error != 0 || len(torrents[i].Files) != 1 || torrents[i].Files[0].Name != want {
				t.Errorf("unaffected transfer %d: %+v", i, torrents[i])
			}
		}
		bad := torrents[2]
		if bad.ID != 303 || bad.Error != trErrorLocal || !strings.Contains(bad.ErrorString, "missing-root") || bad.Files == nil || len(bad.Files) != 0 || bad.Status != trStatusStopped || bad.SeedIdleMode != transmissionLimitModeUnlimited {
			t.Errorf("unsafe transfer did not retain actionable per-transfer failure: %+v", bad)
		}
	}
	for _, path := range []string{"old-root/file.epub", "new-root/other.epub", ".plundrio-files/101.json", ".plundrio-files/202.json", ".plundrio-files/303.json"} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Errorf("fixture disappeared: %s: %v", path, err)
		}
	}
}

func TestManifestNameDriftMalformedRPC(t *testing.T) {
	root := t.TempDir()
	writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(`{`))
	cfg := &config.Config{TargetDir: root}
	service := &manifestRPCService{Manager: download.New(cfg, nil), transfers: []*putio.Transfer{{ID: 101, Name: "new-root"}}}
	torrents := manifestRPC(t, &Server{cfg: cfg, dlService: service}, `{"fields":["id","files","error","errorString"]}`)
	if len(torrents) != 1 || torrents[0].Error != trErrorLocal || !strings.Contains(torrents[0].ErrorString, "parse transfer file state") || torrents[0].Files == nil || len(torrents[0].Files) != 0 {
		t.Fatalf("corrupt state was hidden as missing: %+v", torrents)
	}
}

// torrent-remove must delete the local root the manifest owns, not whatever
// the remote transfer is currently called.
func TestManifestNameDriftRemoveDeletesOwnedRoot(t *testing.T) {
	root := t.TempDir()
	writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(`[{"name":"old-root/file.epub","length":4}]`))
	writeManifestFixture(t, root, ".plundrio-files/202.json", []byte(`[{"name":"new-root/other.epub","length":5}]`))
	writeManifestFixture(t, root, "old-root/file.epub", []byte("book"))
	writeManifestFixture(t, root, "new-root/other.epub", []byte("other"))

	cfg := &config.Config{TargetDir: root}
	transfers := []*putio.Transfer{
		{ID: 101, Hash: "ABC123", Name: "new-root", PercentDone: 100},
		{ID: 202, Hash: "DEF456", Name: "new-root", PercentDone: 100},
	}
	client := &torrentAddClient{transfers: transfers}
	service := &manifestRPCService{Manager: download.New(cfg, nil), transfers: transfers}
	srv := &Server{cfg: cfg, client: client, dlService: service}

	if _, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[101],"delete-local-data":true}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "old-root")); !os.IsNotExist(err) {
		t.Fatalf("owned local root was not deleted: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "new-root/other.epub")); err != nil || string(data) != "other" {
		t.Fatalf("removal destroyed another transfer's data: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".plundrio-files/202.json")); err != nil {
		t.Fatalf("removal destroyed another transfer's manifest: %v", err)
	}
}

// A legacy manifest whose files all sit below one subdirectory proves no
// boundary between the transfer root and the directories under it, so removal
// must refuse rather than guess: neither the subdirectory nor the parent (which
// holds unmanaged data this transfer never downloaded) may be deleted, and a
// remote name equal to either of them is not evidence.
func TestManifestNameDriftRemoveRefusesAmbiguousLegacyRoot(t *testing.T) {
	for _, name := range []string{"Show renamed", "Show", "Show/S01"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(`[{"name":"Show/S01/ep1.mkv","length":3},{"name":"Show/S01/ep2.mkv","length":3}]`))
			writeManifestFixture(t, root, "Show/S01/ep1.mkv", []byte("one"))
			writeManifestFixture(t, root, "Show/S01/ep2.mkv", []byte("two"))
			writeManifestFixture(t, root, "Show/S02/ep3.mkv", []byte("thr"))
			writeManifestFixture(t, root, "Show/poster.jpg", []byte("art"))
			before := localSnapshot(t, root)

			cfg := &config.Config{TargetDir: root}
			transfers := []*putio.Transfer{{ID: 101, Hash: "ABC123", Name: name, FileID: 501, PercentDone: 100}}
			client := &torrentAddClient{transfers: transfers}
			manager := download.New(cfg, nil)
			srv := &Server{cfg: cfg, client: client, dlService: &manifestRPCService{Manager: manager, transfers: transfers}}

			_, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[101],"delete-local-data":true}`))
			if err == nil || !strings.Contains(err.Error(), "establish local ownership for transfer 101; nothing was removed") || !strings.Contains(err.Error(), `ambiguous legacy manifest root below "Show"`) {
				t.Fatalf("ambiguous legacy root did not refuse removal: %v", err)
			}
			if len(client.deleted) != 0 || len(client.deletedFiles) != 0 {
				t.Fatalf("refused removal still mutated Put.io: transfers=%v files=%v", client.deleted, client.deletedFiles)
			}
			if manager.RemovalPending(101) {
				t.Fatal("refused removal published a removal marker")
			}
			localUnchanged(t, root, before)
		})
	}
}

// A transfer removed before its first local byte has a manifest but no local
// root yet, and an importer may have removed a processed root before Put.io
// renamed the transfer. With the remote identity unchanged, a securely resolved
// absent root means there is nothing to delete, whatever the current name.
func TestTorrentRemoveQueuedTransferBeforeFirstByte(t *testing.T) {
	for _, name := range []string{"queued-root", "new-root"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			const manifest = `{"version":1,"transferId":101,"localRoot":"queued-root","files":[{"name":"queued-root/file.epub","length":4}]}`
			writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(manifest))
			writeManifestFixture(t, root, "unrelated/keep.epub", []byte("keep"))

			cfg := &config.Config{TargetDir: root}
			transfers := []*putio.Transfer{{ID: 101, Hash: "ABC123", Name: name, FileID: 501, Status: "DOWNLOADING", PercentDone: 0}}
			client := &torrentAddClient{transfers: transfers}
			manager := download.New(cfg, nil)
			srv := &Server{cfg: cfg, client: client, dlService: &manifestRPCService{Manager: manager, transfers: transfers}}

			if _, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[101],"delete-local-data":true}`)); err != nil {
				t.Fatalf("removal refused with no local root: %v", err)
			}
			if len(client.deleted) != 1 || client.deleted[0] != 101 {
				t.Fatalf("removal did not delete the remote transfer: %v", client.deleted)
			}
			for _, dir := range []string{"queued-root", "new-root"} {
				if _, err := os.Lstat(filepath.Join(root, dir)); !os.IsNotExist(err) {
					t.Fatalf("removal created local root %q: %v", dir, err)
				}
			}
			if data, err := os.ReadFile(filepath.Join(root, "unrelated/keep.epub")); err != nil || string(data) != "keep" {
				t.Fatalf("removal deleted unrelated local data: %q %v", data, err)
			}
		})
	}
}

// Ownership is resolved before anything is mutated: a transfer whose local root
// cannot be established keeps its remote record, its local state and its files.
func TestTorrentRemoveRefusesBeforeAnyMutation(t *testing.T) {
	root := t.TempDir()
	const corrupt = `[{"name":"../escape/file.epub","length":4}]`
	writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(corrupt))
	writeManifestFixture(t, root, "books/old-root/file.epub", []byte("book"))

	cfg := &config.Config{TargetDir: root, UseCategoriesTarget: true}
	manager := download.New(cfg, nil)
	manager.SetCategory(101, "books")
	transfers := []*putio.Transfer{{ID: 101, Hash: "ABC123", Name: "new-root", FileID: 501, Status: "DOWNLOADING", PercentDone: 100}}
	client := &torrentAddClient{transfers: transfers}
	srv := &Server{cfg: cfg, client: client, dlService: &manifestRPCService{Manager: manager, transfers: transfers}}

	_, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[101],"delete-local-data":true}`))
	if err == nil || !strings.Contains(err.Error(), "establish local ownership for transfer 101; nothing was removed") {
		t.Fatalf("unresolved ownership did not refuse removal: %v", err)
	}
	if len(client.deleted) != 0 || len(client.deletedFiles) != 0 {
		t.Fatalf("refused removal still mutated Put.io: transfers=%v files=%v", client.deleted, client.deletedFiles)
	}
	if got := manager.GetCategory(101); got != "books" {
		t.Fatalf("category = %q, want it retained", got)
	}
	if manager.RemovalPending(101) {
		t.Fatal("refused removal published a removal marker")
	}
	entries, err := os.ReadDir(filepath.Join(root, ".plundrio-files"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "101.json" {
		t.Fatalf("local ownership state changed: %+v %v", entries, err)
	}
	if data, err := os.ReadFile(filepath.Join(root, ".plundrio-files/101.json")); err != nil || string(data) != corrupt {
		t.Fatalf("manifest rewritten: %q %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "books/old-root/file.epub")); err != nil || string(data) != "book" {
		t.Fatalf("local payload changed: %q %v", data, err)
	}
	// The transfer stays visible, so the operator can retry the same request.
	if torrents := manifestRPC(t, srv, `{"ids":[101],"fields":["id","name"]}`); len(torrents) != 1 || torrents[0].ID != 101 {
		t.Fatalf("refused removal dropped the transfer record: %+v", torrents)
	}
}

func localSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	entries := make(map[string]string)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			entries[path] = info.Mode().String()
			return nil
		}
		data, err := os.ReadFile(path)
		entries[path] = info.Mode().String() + ":" + string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func localUnchanged(t *testing.T, root string, before map[string]string) {
	t.Helper()
	after := localSnapshot(t, root)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("local filesystem changed:\nbefore=%v\nafter=%v", before, after)
	}
}

// A transfer with no manifest owns no local data. Removal still falls back to
// its remote name, so that candidate must clear every recorded claim first:
// deleting another transfer's payload is never a valid interpretation of an
// absent manifest, while an unclaimed name stays removable as before.
func TestTorrentRemoveManifestlessTransferRefusesOwnedRoot(t *testing.T) {
	root := t.TempDir()
	writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(`[{"name":"Show/ep1.mkv","length":3}]`))
	writeManifestFixture(t, root, "Show/ep1.mkv", []byte("one"))
	writeManifestFixture(t, root, "Solo/leftover.bin", []byte("old"))
	before := localSnapshot(t, root)

	cfg := &config.Config{TargetDir: root}
	transfers := []*putio.Transfer{
		{ID: 101, Hash: "ABC123", Name: "Show", FileID: 501, Status: "COMPLETED", PercentDone: 100},
		{ID: 202, Hash: "DEF456", Name: "Show", FileID: 502, Status: "DOWNLOADING"},
		{ID: 303, Hash: "AAA789", Name: "Solo", FileID: 503, Status: "DOWNLOADING"},
	}
	client := &torrentAddClient{transfers: transfers}
	manager := download.New(cfg, nil)
	srv := &Server{cfg: cfg, client: client, dlService: &manifestRPCService{Manager: manager, transfers: transfers}}

	_, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[202],"delete-local-data":true}`))
	if err == nil || !strings.Contains(err.Error(), "establish local ownership for transfer 202; nothing was removed") || !strings.Contains(err.Error(), "collides with transfer 101") {
		t.Fatalf("manifest-less removal did not refuse another transfer's root: %v", err)
	}
	if len(client.deleted) != 0 || len(client.deletedFiles) != 0 {
		t.Fatalf("refused removal still mutated Put.io: transfers=%v files=%v", client.deleted, client.deletedFiles)
	}
	if manager.RemovalPending(202) {
		t.Fatal("refused removal published a removal marker")
	}
	localUnchanged(t, root, before)

	// Control: the same manifest-less path still removes an unclaimed root.
	if _, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[303],"delete-local-data":true}`)); err != nil {
		t.Fatalf("manifest-less removal of an unclaimed root refused: %v", err)
	}
	if len(client.deleted) != 1 || client.deleted[0] != 303 {
		t.Fatalf("unclaimed removal did not delete the remote transfer: %v", client.deleted)
	}
	if _, err := os.Lstat(filepath.Join(root, "Solo")); !os.IsNotExist(err) {
		t.Fatalf("unclaimed local root survived removal: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "Show/ep1.mkv")); err != nil || string(data) != "one" {
		t.Fatalf("removal touched the owned root: %q %v", data, err)
	}
}

// A stored record naming several local roots cannot say what it owns. Every
// transfer reachable through those roots must fail closed, over both selector
// forms and through removal, rather than one of them quietly taking a root the
// broken record still holds files in.
func TestManifestMalformedMultipleRootsRPCAndRemoval(t *testing.T) {
	for _, malformed := range []string{
		`[{"name":"a/first.epub","length":3},{"name":"b/owned.epub","length":3}]`,
		`[{"name":"b/owned.epub","length":3},{"name":"a/first.epub","length":3}]`,
	} {
		t.Run(malformed, func(t *testing.T) {
			root := t.TempDir()
			writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(malformed))
			writeManifestFixture(t, root, ".plundrio-files/202.json", []byte(`[{"name":"b/second.epub","length":3}]`))
			writeManifestFixture(t, root, "a/first.epub", []byte("one"))
			writeManifestFixture(t, root, "b/owned.epub", []byte("two"))
			writeManifestFixture(t, root, "b/second.epub", []byte("thr"))
			before := localSnapshot(t, root)

			cfg := &config.Config{TargetDir: root}
			transfers := []*putio.Transfer{
				{ID: 101, Hash: "ABC123", Name: "a", FileID: 501, Status: "COMPLETED", PercentDone: 100},
				{ID: 202, Hash: "DEF456", Name: "b", FileID: 502, Status: "COMPLETED", PercentDone: 100},
			}
			client := &torrentAddClient{transfers: transfers}
			manager := download.New(cfg, nil)
			srv := &Server{cfg: cfg, client: client, dlService: &manifestRPCService{Manager: manager, transfers: transfers}}

			for _, ids := range []string{``, `"ids":[101,"def456"],`} {
				torrents := manifestRPC(t, srv, `{`+ids+`"fields":["id","name","files","error","errorString"]}`)
				if len(torrents) != 2 {
					t.Fatalf("lost transfers: %+v", torrents)
				}
				for _, torrent := range torrents {
					if torrent.Error != trErrorLocal || !strings.Contains(torrent.ErrorString, "share one local root") || torrent.Files == nil || len(torrent.Files) != 0 {
						t.Fatalf("malformed inventory was not reported: %+v", torrent)
					}
				}
				if !strings.Contains(torrents[1].ErrorString, "manifest 101") {
					t.Fatalf("competing transfer did not name the broken record: %+v", torrents[1])
				}
			}

			_, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[202],"delete-local-data":true}`))
			if err == nil || !strings.Contains(err.Error(), "establish local ownership for transfer 202; nothing was removed") || !strings.Contains(err.Error(), "manifest 101") {
				t.Fatalf("removal proceeded against a malformed competing record: %v", err)
			}
			if len(client.deleted) != 0 || len(client.deletedFiles) != 0 {
				t.Fatalf("refused removal still mutated Put.io: transfers=%v files=%v", client.deleted, client.deletedFiles)
			}
			if manager.RemovalPending(202) {
				t.Fatal("refused removal published a removal marker")
			}
			localUnchanged(t, root, before)
		})
	}
}

// With category subfolders the deletion target is TargetDir/<category>/<name>,
// so the manifest-less ownership preflight must compare claims in that same
// layout: a bare-name comparison would clear a root another transfer owns.
func TestTorrentRemoveManifestlessTransferRefusesCategoryRoot(t *testing.T) {
	root := t.TempDir()
	writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(`[{"name":"Show/ep1.mkv","length":3}]`))
	writeManifestFixture(t, root, "tv/Show/ep1.mkv", []byte("one"))
	writeManifestFixture(t, root, "tv/Solo/leftover.bin", []byte("old"))

	cfg := &config.Config{TargetDir: root, UseCategoriesTarget: true}
	transfers := []*putio.Transfer{
		{ID: 101, Hash: "ABC123", Name: "Show", FileID: 501, Status: "COMPLETED", PercentDone: 100},
		{ID: 202, Hash: "DEF456", Name: "Show", FileID: 502, Status: "DOWNLOADING"},
		{ID: 303, Hash: "AAA789", Name: "Solo", FileID: 503, Status: "DOWNLOADING"},
	}
	client := &torrentAddClient{transfers: transfers}
	manager := download.New(cfg, nil)
	for _, id := range []int64{101, 202, 303} {
		manager.SetCategory(id, "tv")
	}
	srv := &Server{cfg: cfg, client: client, dlService: &manifestRPCService{Manager: manager, transfers: transfers}}
	before := localSnapshot(t, root)

	for _, ids := range []string{``, `"ids":[101,"def456"],`} {
		torrents := manifestRPC(t, srv, `{`+ids+`"fields":["id","name","files","error","errorString"]}`)
		if torrents[0].Error != 0 || len(torrents[0].Files) != 1 || torrents[0].Files[0].Name != "Show/ep1.mkv" {
			t.Fatalf("owning transfer lost its files: %+v", torrents[0])
		}
		if torrents[1].Error != trErrorLocal || !strings.Contains(torrents[1].ErrorString, "collides with transfer 101") || torrents[1].Files == nil || len(torrents[1].Files) != 0 {
			t.Fatalf("manifest-less transfer claimed a categorised root: %+v", torrents[1])
		}
	}

	_, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[202],"delete-local-data":true}`))
	if err == nil || !strings.Contains(err.Error(), "establish local ownership for transfer 202; nothing was removed") || !strings.Contains(err.Error(), "collides with transfer 101") {
		t.Fatalf("manifest-less removal ignored the category layout: %v", err)
	}
	if len(client.deleted) != 0 || len(client.deletedFiles) != 0 {
		t.Fatalf("refused removal still mutated Put.io: transfers=%v files=%v", client.deleted, client.deletedFiles)
	}
	if manager.RemovalPending(202) || manager.GetCategory(202) != "tv" {
		t.Fatalf("refused removal dropped local bookkeeping: pending=%t category=%q", manager.RemovalPending(202), manager.GetCategory(202))
	}
	localUnchanged(t, root, before)

	// Control: an unclaimed root inside the same category is still removed.
	if _, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[303],"delete-local-data":true}`)); err != nil {
		t.Fatalf("manifest-less removal of an unclaimed categorised root refused: %v", err)
	}
	if len(client.deleted) != 1 || client.deleted[0] != 303 {
		t.Fatalf("unclaimed removal did not delete the remote transfer: %v", client.deleted)
	}
	if _, err := os.Lstat(filepath.Join(root, "tv/Solo")); !os.IsNotExist(err) {
		t.Fatalf("unclaimed local root survived removal: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "tv/Show/ep1.mkv")); err != nil || string(data) != "one" {
		t.Fatalf("removal touched the owned root: %q %v", data, err)
	}
}

// A remote name that resolves nowhere inside the download root cannot be shown
// to own anything, so the whole request is refused before the remote records
// are deleted rather than after, when only the local step could still fail.
func TestTorrentRemoveManifestlessUnsafeNameRefusesEverything(t *testing.T) {
	for _, name := range []string{"", ".", "../escape", ".plundrio-files"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(`{`))
			writeManifestFixture(t, root, "keep/file.epub", []byte("keep"))

			cfg := &config.Config{TargetDir: root, UseCategoriesTarget: true}
			transfers := []*putio.Transfer{{ID: 202, Hash: "DEF456", Name: name, FileID: 502, Status: "DOWNLOADING"}}
			client := &torrentAddClient{transfers: transfers}
			manager := download.New(cfg, nil)
			manager.SetCategory(202, "tv")
			srv := &Server{cfg: cfg, client: client, dlService: &manifestRPCService{Manager: manager, transfers: transfers}}
			before := localSnapshot(t, root)

			_, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[202],"delete-local-data":true}`))
			if err == nil || !strings.Contains(err.Error(), "establish local ownership for transfer 202; nothing was removed") {
				t.Fatalf("unsafe manifest-less name did not refuse removal: %v", err)
			}
			if len(client.deleted) != 0 || len(client.deletedFiles) != 0 {
				t.Fatalf("refused removal still mutated Put.io: transfers=%v files=%v", client.deleted, client.deletedFiles)
			}
			if manager.RemovalPending(202) || manager.GetCategory(202) != "tv" {
				t.Fatalf("refused removal dropped local bookkeeping: pending=%t category=%q", manager.RemovalPending(202), manager.GetCategory(202))
			}
			localUnchanged(t, root, before)
		})
	}
}

// An unreadable removal marker leaves a released transfer's category unknown
// while its payload stays on disk. Ownership must refuse rather than compare
// against the download root, which would clear the way to delete that payload.
func TestTorrentRemoveUnreadableRemovalMarkerRefusesCategoryRoot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		marker  string
		wantErr string
	}{
		{name: "corrupt marker", marker: `{`, wantErr: "category for manifest 101"},
		{name: "readable marker", marker: `"tv"`, wantErr: "collides with transfer 101"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(`[{"name":"Show/ep1.mkv","length":3}]`))
			writeManifestFixture(t, root, ".plundrio-files/101.removing.json", []byte(tc.marker))
			writeManifestFixture(t, root, "tv/Show/ep1.mkv", []byte("one"))

			cfg := &config.Config{TargetDir: root, UseCategoriesTarget: true}
			transfers := []*putio.Transfer{
				{ID: 101, Hash: "ABC123", Name: "Show", FileID: 501, Status: "COMPLETED", PercentDone: 100},
				{ID: 202, Hash: "DEF456", Name: "Show", FileID: 502, Status: "DOWNLOADING"},
			}
			client := &torrentAddClient{transfers: transfers}
			manager := download.New(cfg, nil)
			manager.SetCategory(101, "tv")
			manager.SetCategory(202, "tv")
			srv := &Server{cfg: cfg, client: client, dlService: &manifestRPCService{Manager: manager, transfers: transfers}}
			before := localSnapshot(t, root)

			torrents := manifestRPC(t, srv, `{"fields":["id","name","files","error","errorString"]}`)
			if torrents[1].Error != trErrorLocal || !strings.Contains(torrents[1].ErrorString, tc.wantErr) {
				t.Fatalf("manifest-less transfer was not refused: %+v", torrents[1])
			}

			_, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[202],"delete-local-data":true}`))
			if err == nil || !strings.Contains(err.Error(), "establish local ownership for transfer 202; nothing was removed") || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("removal proceeded against an unresolved owner category: %v", err)
			}
			if len(client.deleted) != 0 || len(client.deletedFiles) != 0 {
				t.Fatalf("refused removal still mutated Put.io: transfers=%v files=%v", client.deleted, client.deletedFiles)
			}
			if manager.RemovalPending(202) || manager.GetCategory(202) != "tv" {
				t.Fatalf("refused removal dropped local bookkeeping: pending=%t category=%q", manager.RemovalPending(202), manager.GetCategory(202))
			}
			localUnchanged(t, root, before)
		})
	}
}

// The literal name Put.io reported survives the whole RPC round trip: it is
// what torrent-get reports and what torrent-remove deletes, with no rewriting.
func TestTorrentGetAndRemoveLiteralBackslashName(t *testing.T) {
	const name = `AC\DC - Album`
	root := t.TempDir()
	writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(`[{"name":"AC\\DC - Album/file.mkv","length":3}]`))
	writeManifestFixture(t, root, filepath.Join(name, "file.mkv"), []byte("one"))
	writeManifestFixture(t, root, "keep/other.mkv", []byte("two"))

	cfg := &config.Config{TargetDir: root}
	transfers := []*putio.Transfer{{ID: 101, Hash: "ABC123", Name: "renamed upstream", FileID: 501, Status: "DOWNLOADING", PercentDone: 100}}
	client := &torrentAddClient{transfers: transfers}
	manager := download.New(cfg, nil)
	service := &manifestRPCService{
		Manager:   manager,
		transfers: transfers,
		contexts: map[int64]*download.TransferContext{
			101: download.NewTransferContext(101, 0, download.TransferLifecycleProcessed),
		},
	}
	srv := &Server{cfg: cfg, client: client, dlService: service}

	torrents := manifestRPC(t, srv, `{"fields":["id","name","files","error","errorString"]}`)
	if len(torrents) != 1 || torrents[0].Error != 0 || torrents[0].Name != name {
		t.Fatalf("literal name was not reported as the local root: %+v", torrents[0])
	}
	if len(torrents[0].Files) != 1 || torrents[0].Files[0].Name != name+"/file.mkv" {
		t.Fatalf("literal name was rewritten in the file list: %+v", torrents[0].Files)
	}

	if _, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[101],"delete-local-data":true}`)); err != nil {
		t.Fatalf("removal of a literal-backslash root refused: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
		t.Fatalf("owned literal root survived removal: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "keep/other.mkv")); err != nil || string(data) != "two" {
		t.Fatalf("removal reached beyond the literal root: %q %v", data, err)
	}
}
