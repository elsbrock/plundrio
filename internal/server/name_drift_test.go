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
	if ctx, ok := s.contexts[id]; ok {
		return ctx, true
	}
	return s.Manager.GetTransferContext(id)
}

type manifestRPCTorrent struct {
	ID          int64
	Name        string
	HashString  string
	Error       int
	ErrorString string
	Status      int
	Files       []transmissionFile
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
					// The import root is also stable when files are not requested.
					ids := manifestRPC(t, srv, fmt.Sprintf(`{"ids":[%s],"fields":["id","name","hashString"]}`, selector))
					if len(ids) != 1 || ids[0].ID != 101 || ids[0].Name != "old-root" {
						t.Fatalf("ID-only control = %+v", ids)
					}
					got := manifestRPC(t, srv, fmt.Sprintf(`{"ids":[%s],"fields":["id","name","hashString","files","error","errorString"]}`, selector))
					want := []transmissionFile{{Name: "old-root/file.epub", Length: 4}}
					if len(got) != 1 || got[0].ID != 101 || got[0].Name != "old-root" || got[0].HashString != "ABC123" || got[0].Error != 0 || !reflect.DeepEqual(got[0].Files, want) {
						t.Fatalf("files-inclusive response = %+v, want unchanged old-root files %+v", got, want)
					}
				})
			}
		})
	}
	after, err := os.Stat(filepath.Join(root, "old-root/file.epub"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("payload moved or replaced: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".plundrio-files/101.json"))
	if err != nil || string(data) != manifest {
		t.Fatalf("manifest changed: %q, %v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "new-root")); !os.IsNotExist(err) {
		t.Fatalf("remote name created a local root: %v", err)
	}
}

func TestTorrentRemoveUsesSavedRoot(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{TargetDir: root, UseCategoriesTarget: true}
	manager := download.New(cfg, nil)
	manager.SetCategory(101, "books")
	writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(`[{"name":"old-root/file.epub","length":4}]`))
	writeManifestFixture(t, root, "books/old-root/file.epub", []byte("book"))
	writeManifestFixture(t, root, "books/new-root/file.epub", []byte("other"))
	client := &pruningRemovalClient{
		torrentAddClient: &torrentAddClient{transfers: []*putio.Transfer{{ID: 101, Name: "new-root", Status: "DOWNLOADING"}}},
		manager:          manager,
	}
	srv := &Server{cfg: cfg, client: client, dlService: manager}
	if _, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[101],"delete-local-data":true}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "books/old-root")); !os.IsNotExist(err) {
		t.Fatalf("saved root remains: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "books/new-root/file.epub")); err != nil || string(data) != "other" {
		t.Fatalf("remote-name directory changed: %q, %v", data, err)
	}
}

func TestBadManifestDoesNotEmptyTorrentGet(t *testing.T) {
	for _, bad := range []string{
		`not json`, `[]`,
		`[{"name":"bad/file.epub","length":-1}]`,
		`[{"name":"../file.epub","length":4}]`,
		`[{"name":"bad/file.epub","length":4},{"name":"other/file.epub","length":4}]`,
	} {
		t.Run(bad, func(t *testing.T) {
			root := t.TempDir()
			cfg := &config.Config{TargetDir: root}
			writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(bad))
			writeManifestFixture(t, root, ".plundrio-files/202.json", []byte(`[{"name":"saved/file.epub","length":4}]`))
			service := &manifestRPCService{Manager: download.New(cfg, nil), transfers: []*putio.Transfer{
				{ID: 101, Name: "bad", Status: "COMPLETED"},
				{ID: 202, Name: "renamed", Status: "COMPLETED"},
			}}
			got := manifestRPC(t, &Server{cfg: cfg, dlService: service}, `{"fields":["id","name","files","status","error","errorString"]}`)
			if len(got) != 2 {
				t.Fatalf("queue lost transfers: %+v", got)
			}
			if got[0].ID != 101 || got[0].Error != trErrorLocal || got[0].ErrorString == "" || got[0].Status != trStatusStopped || got[0].Files == nil || len(got[0].Files) != 0 {
				t.Fatalf("bad transfer = %+v", got[0])
			}
			if got[1].ID != 202 || got[1].Error != 0 || got[1].Name != "saved" || len(got[1].Files) != 1 {
				t.Fatalf("healthy transfer = %+v", got[1])
			}
			if bad == "not json" || bad == "[]" {
				t.Run("without files", func(t *testing.T) {
					got := manifestRPC(t, &Server{cfg: cfg, dlService: service}, `{"fields":["id","name","status","error","errorString"]}`)
					if len(got) != 2 || got[0].ID != 101 || got[0].Error != trErrorLocal || got[0].Status != trStatusStopped || got[0].ErrorString == "" {
						t.Fatalf("bad manifest without files = %+v", got)
					}
					if got[1].ID != 202 || got[1].Error != 0 || got[1].Name != "saved" {
						t.Fatalf("healthy transfer without files = %+v", got[1])
					}
				})
			}
		})
	}
}

func TestTorrentGetAllowsImportedFilesAfterRename(t *testing.T) {
	for _, state := range []download.TransferLifecycleState{download.TransferLifecycleCompleted, download.TransferLifecycleProcessed} {
		t.Run(state.String(), func(t *testing.T) {
			root := t.TempDir()
			cfg := &config.Config{TargetDir: root}
			// The importer has moved the payload; only its manifest remains.
			writeManifestFixture(t, root, ".plundrio-files/101.json", []byte(`[{"name":"old-root/file.epub","length":4}]`))
			ctx := download.NewTransferContext(101, 1, state)
			ctx.SetTotalSize(4)
			ctx.AddDownloadedBytes(4)
			service := &manifestRPCService{Manager: download.New(cfg, nil),
				transfers: []*putio.Transfer{{ID: 101, Name: "new-root", Status: "COMPLETED", PercentDone: 100, Size: 4}},
				contexts:  map[int64]*download.TransferContext{101: ctx},
			}
			for _, fields := range []string{`["id","name","error","status"]`, `["id","name","error","status","files"]`} {
				got := manifestRPC(t, &Server{cfg: cfg, dlService: service}, `{"fields":`+fields+`}`)
				if len(got) != 1 || got[0].Name != "old-root" || got[0].Error != 0 {
					t.Fatalf("imported transfer = %+v", got)
				}
				if strings.Contains(fields, "files") && (len(got[0].Files) != 1 || got[0].Files[0].BytesCompleted != 4) {
					t.Fatalf("completed files = %+v", got[0].Files)
				}
			}
		})
	}
}
