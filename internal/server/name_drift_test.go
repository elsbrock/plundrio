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
	"sync"
	"testing"
	"testing/synctest"
	"time"

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

// Run with: go test ./internal/server -run 'ManifestNameDrift' -count=1 -v
func TestManifestNameDriftRPC(t *testing.T) {
	for _, tc := range []struct {
		name, root, category string
		explicit, pending    bool
	}{
		{name: "legacy", root: "old-root"},
		{name: "explicit", root: "old-root", explicit: true},
		{name: "nested", root: "old-root/book", explicit: true},
		{name: "category", root: "old-root", category: "books"},
		{name: "literal backslash", root: `old\root`},
		{name: "incomplete", root: "old-root", pending: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dir := t.TempDir()
				file := tc.root + "/file.epub"
				data := fmt.Sprintf(`[{"name":%q,"length":4}]`, file)
				if tc.explicit {
					data = fmt.Sprintf(`{"version":1,"transferId":101,"localRoot":%q,"files":%s}`, tc.root, data)
				}
				writeManifestFixture(t, dir, ".plundrio-files/101.json", []byte(data))
				payload := "book"
				if tc.pending {
					payload = "bo"
				}
				writeManifestFixture(t, dir, filepath.Join(tc.category, file), []byte(payload))
				writeManifestFixture(t, dir, ".plundrio-files/202.json", []byte(`[{"name":"other-root/other.epub","length":5}]`))
				writeManifestFixture(t, dir, "other-root/other.epub", []byte("other"))
				cfg := &config.Config{TargetDir: dir, FolderID: 9, WorkerCount: 1, UseCategoriesTarget: tc.category != ""}
				client := &nameDriftClient{name: tc.root, hash: "ABC123", pending: tc.pending}
				manager := download.New(cfg, client)
				if tc.category != "" {
					manager.SetCategory(101, tc.category)
				}
				before := nameDriftSnapshot(t, dir)
				manager.Start()
				defer func() { manager.Stop() }()
				for _, phase := range []string{"initial", "renamed poll", "restart"} {
					if phase == "renamed poll" {
						client.rename("new-root", "DEF123")
						time.Sleep(30 * time.Second)
					}
					if phase == "restart" {
						manager.Stop()
						manager = download.New(cfg, client)
						manager.Start()
					}
					synctest.Wait()
					state, ok := manager.GetTransferContext(101)
					want := download.TransferLifecycleProcessed
					if tc.pending {
						want = download.TransferLifecycleDownloading
					}
					if !ok || state.Name != tc.root || state.GetState() != want {
						t.Fatalf("%s context: %+v", phase, state)
					}
					srv := &Server{cfg: cfg, dlService: manager}
					for _, ids := range []string{`[101]`, `["` + strings.ToLower(client.hash) + `"]`, `[101,"bbb222"]`, `null`} {
						for _, fields := range []string{`["id","name"]`, `["id","name","files","error"]`} {
							got := manifestRPC(t, srv, `{"ids":`+ids+`,"fields":`+fields+`}`)
							count := 1
							if ids == `null` || strings.Contains(ids, "bbb222") {
								count = 2
							}
							if len(got) != count || got[0].ID != 101 || got[0].Name != tc.root || got[0].HashString != client.hash || got[0].Error != 0 {
								t.Fatalf("%s %s: %+v", phase, ids, got)
							}
							if strings.Contains(fields, "files") {
								bytes := int64(4)
								if tc.pending {
									bytes = 0
								}
								want := []transmissionFile{{Name: filepath.ToSlash(file), Length: 4, BytesCompleted: bytes}}
								if !reflect.DeepEqual(got[0].Files, want) {
									t.Fatalf("%s files: %+v", phase, got[0].Files)
								}
							}
							if count == 2 && (got[1].ID != 202 || got[1].Error != 0 || got[1].Name != "other-root") {
								t.Fatalf("unrelated transfer: %+v", got[1])
							}
						}
					}
					nameDriftUnchanged(t, dir, before)
				}
			})
		})
	}
}

type nameDriftClient struct {
	download.PutioClient
	mu         sync.Mutex
	name, hash string
	pending    bool
	files      []*putio.File
}

func (c *nameDriftClient) rename(name, hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.name, c.hash = name, hash
}
func (c *nameDriftClient) GetTransfers(context.Context) ([]*putio.Transfer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fileID := int64(0)
	if c.pending {
		fileID = 501
	}
	return []*putio.Transfer{
		{ID: 101, Name: c.name, Hash: c.hash, Status: "COMPLETED", PercentDone: 100, Size: 4, FileID: fileID, SaveParentID: 9},
		{ID: 202, Name: "other-root", Hash: "BBB222", Status: "COMPLETED", PercentDone: 100, Size: 5, SaveParentID: 9},
	}, nil
}
func (c *nameDriftClient) GetAllTransferFiles(context.Context, int64) ([]*putio.File, error) {
	if c.files != nil {
		return c.files, nil
	}
	return []*putio.File{{ID: 601, Name: "file.epub", Size: 4}}, nil
}
func (c *nameDriftClient) GetDownloadURL(ctx context.Context, _ int64) (string, error) {
	<-ctx.Done() // Leave the partial file untouched while exercising queueing and polling.
	return "", ctx.Err()
}

// Refused records and allowed renames must both leave ownership and files intact.
func TestManifestNameDriftSafety(t *testing.T) {
	const valid = `[{"name":"old-root/file.epub","length":4}]`
	for _, tc := range []struct{ name, data, other, want string }{
		{name: "legacy", data: valid},
		{name: "multiple files", data: `[{"name":"old-root/file.epub","length":4},{"name":"old-root/book/file.epub","length":4}]`},
		{name: "nested explicit", data: `{"version":1,"transferId":101,"localRoot":"old-root/book","files":[{"name":"old-root/book/file.epub","length":4}]}`},
		{name: "historical hash ignored", data: `{"version":1,"transferId":101,"hash":"OLD","localRoot":"old-root","files":[{"name":"old-root/file.epub","length":4}]}`},
		{name: "malformed", data: `{`, want: "parse"},
		{name: "empty", data: `[]`, want: "empty"},
		{name: "null", data: `null`, want: "invalid"},
		{name: "foreign ID", data: `{"version":1,"transferId":202,"localRoot":"old-root","files":[{"name":"old-root/file.epub","length":4}]}`, want: "invalid"},
		{name: "wrong version", data: `{"version":2,"transferId":101,"localRoot":"old-root","files":[{"name":"old-root/file.epub","length":4}]}`, want: "invalid"},
		{name: "wrong root", data: `{"version":1,"transferId":101,"localRoot":"other-root","files":[{"name":"old-root/file.epub","length":4}]}`, want: "outside local root"},
		{name: "ambiguous legacy", data: `[{"name":"old-root/book/file.epub","length":4}]`, want: "ambiguous"},
		{name: "ambiguous siblings", data: `[{"name":"old-root/book/file.epub","length":4},{"name":"old-root/art/file.epub","length":4}]`, want: "ambiguous"},
		{name: "multiple roots", data: `[{"name":"old-root/file.epub","length":4},{"name":"other-root/file.epub","length":4}]`, want: "share one local root"},
		{name: "duplicate", data: `[{"name":"old-root/file.epub","length":4},{"name":"old-root/file.epub","length":4}]`, want: "duplicate"},
		{name: "traversal", data: `[{"name":"../escape/file.epub","length":4}]`, want: "unsafe"},
		{name: "internal traversal", data: `[{"name":"old-root/book/../file.epub","length":4}]`, want: "unsafe"},
		{name: "absolute", data: `[{"name":"/escape/file.epub","length":4}]`, want: "unsafe"},
		{name: "reserved", data: `[{"name":".PLUNDRIO-FILES/file.epub","length":4}]`, want: "unsafe"},
		{name: "nul", data: `[{"name":"old-root/\u0000file.epub","length":4}]`, want: "unsafe"},
		{name: "negative size", data: `[{"name":"old-root/file.epub","length":-1}]`, want: "unsafe"},
		{name: "collision", data: valid, other: `[{"name":"old-root/other.epub","length":5}]`, want: "collides"},
		{name: "case collision", data: valid, other: `[{"name":"OLD-ROOT/other.epub","length":5}]`, want: "collides"},
		{name: "ambiguous claimant", data: valid, other: `[{"name":"old-root/nested/other.epub","length":5}]`, want: "collides"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeManifestFixture(t, dir, ".plundrio-files/101.json", []byte(tc.data))
			for _, f := range []string{"old-root/file.epub", "old-root/book/file.epub", "old-root/art/file.epub"} {
				writeManifestFixture(t, dir, f, []byte("book"))
			}
			if tc.other != "" {
				writeManifestFixture(t, dir, ".plundrio-files/202.json", []byte(tc.other))
			}
			before := nameDriftSnapshot(t, dir)
			m := download.New(&config.Config{TargetDir: dir}, nil)
			for _, mode := range []download.ManifestCheck{download.ManifestCheckPending, download.ManifestCheckProcessed, download.ManifestCheckComplete} {
				manifest, err := m.GetTransferManifest(&putio.Transfer{ID: 101, Name: "new-root", Hash: "NEW"}, mode)
				if tc.want == "" {
					if err != nil || manifest.LocalRoot == "new-root" {
						t.Fatalf("%d: %+v %v", mode, manifest, err)
					}
				} else if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("%d: got %v, want %s", mode, err, tc.want)
				}
			}
			nameDriftUnchanged(t, dir, before)
		})
	}
}

func TestManifestNameDriftPaths(t *testing.T) {
	for _, path := range []string{"old-root", "old-root/file.epub", ".plundrio-files", ".plundrio-files/101.json"} {
		for _, symlink := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/symlink=%t", path, symlink), func(t *testing.T) {
				dir := t.TempDir()
				writeManifestFixture(t, dir, ".plundrio-files/101.json", []byte(`[{"name":"old-root/file.epub","length":4}]`))
				writeManifestFixture(t, dir, "old-root/file.epub", []byte("book"))
				if err := os.RemoveAll(filepath.Join(dir, path)); err != nil {
					t.Fatal(err)
				}
				if symlink {
					if err := os.Symlink(t.TempDir(), filepath.Join(dir, path)); err != nil {
						t.Fatal(err)
					}
				}
				before := nameDriftSnapshot(t, dir)
				m := download.New(&config.Config{TargetDir: dir}, nil)
				manifest, err := m.GetTransferManifest(&putio.Transfer{ID: 101, Name: "new-root"}, download.ManifestCheckComplete)
				absentManifest := !symlink && strings.HasPrefix(path, ".plundrio-files")
				if absentManifest {
					if err != nil || len(manifest.Files) != 0 {
						t.Fatalf("absent record adopted files: %+v %v", manifest, err)
					}
				} else if err == nil {
					t.Fatal("unsafe or missing path accepted")
				}
				nameDriftUnchanged(t, dir, before)
			})
		}
	}
}

func TestManifestNameDriftMixedRPC(t *testing.T) {
	dir := t.TempDir()
	for id, root := range map[int]string{101: "old-root", 202: "other-root", 303: "missing-root"} {
		writeManifestFixture(t, dir, fmt.Sprintf(".plundrio-files/%d.json", id), []byte(fmt.Sprintf(`[{"name":"%s/file.epub","length":4}]`, root)))
		if id != 303 {
			writeManifestFixture(t, dir, root+"/file.epub", []byte("book"))
		}
	}
	cfg := &config.Config{TargetDir: dir}
	service := &manifestRPCService{Manager: download.New(cfg, nil), transfers: []*putio.Transfer{
		{ID: 101, Hash: "ABC", Name: "new-root"}, {ID: 202, Hash: "DEF", Name: "new-root"}, {ID: 303, Hash: "BAD", Name: "bad-new"},
	}}
	before := nameDriftSnapshot(t, dir)
	for _, ids := range []string{`null`, `[101,"def",303]`} {
		got := manifestRPC(t, &Server{cfg: cfg, dlService: service}, `{"ids":`+ids+`,"fields":["id","name","files","error"]}`)
		if len(got) != 3 || got[0].Error != 0 || got[1].Error != 0 || got[0].Files[0].Name != "old-root/file.epub" || got[1].Files[0].Name != "other-root/file.epub" || got[2].Error != trErrorLocal || len(got[2].Files) != 0 {
			t.Fatalf("mixed response: %+v", got)
		}
	}
	nameDriftUnchanged(t, dir, before)
}

type nameDriftEntry struct {
	info os.FileInfo
	data string
}

func nameDriftSnapshot(t *testing.T, root string) map[string]nameDriftEntry {
	t.Helper()
	result := map[string]nameDriftEntry{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		entry := nameDriftEntry{info: info}
		if info.Mode()&os.ModeSymlink != 0 {
			entry.data, err = os.Readlink(path)
		} else if info.Mode().IsRegular() {
			var data []byte
			data, err = os.ReadFile(path)
			entry.data = string(data)
		}
		result[path] = entry
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func nameDriftUnchanged(t *testing.T, root string, before map[string]nameDriftEntry) {
	t.Helper()
	after := nameDriftSnapshot(t, root)
	if len(before) != len(after) {
		t.Fatalf("entry count changed: %d -> %d", len(before), len(after))
	}
	for path, old := range before {
		now, ok := after[path]
		if !ok || old.data != now.data || !os.SameFile(old.info, now.info) || old.info.Mode() != now.info.Mode() || !old.info.ModTime().Equal(now.info.ModTime()) {
			t.Errorf("changed: %s", path)
		}
	}
}

func TestManifestNameDriftRetry(t *testing.T) {
	for _, tc := range []struct {
		name, remoteFile string
		size             int64
		create, missing  bool
		wantErr          string
	}{
		{name: "same files", remoteFile: "file.epub", size: 4},
		{name: "changed file", remoteFile: "other.epub", size: 4, wantErr: "differs"},
		{name: "changed length", remoteFile: "file.epub", size: 5, wantErr: "differs"},
		{name: "lost old root", remoteFile: "file.epub", size: 4, missing: true, wantErr: "stat manifest"},
		{name: "new nested root", remoteFile: "file.epub", size: 4, create: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dir := t.TempDir()
				root := "old-root"
				if tc.create {
					root = "old-root/book"
				}
				if !tc.create {
					writeManifestFixture(t, dir, ".plundrio-files/101.json", []byte(`[{"name":"old-root/file.epub","length":4}]`))
				}
				if !tc.missing {
					writeManifestFixture(t, dir, root+"/file.epub", []byte("bo"))
				}
				writeManifestFixture(t, dir, ".plundrio-files/202.json", []byte(`[{"name":"other-root/other.epub","length":5}]`))
				writeManifestFixture(t, dir, "other-root/other.epub", []byte("other"))
				cfg := &config.Config{TargetDir: dir, FolderID: 9, WorkerCount: 1}
				client := &nameDriftClient{name: "new-root", hash: "NEW", pending: true, files: []*putio.File{{ID: 601, Name: tc.remoteFile, Size: tc.size}}}
				if tc.create {
					client.name = root
				}
				before := nameDriftSnapshot(t, dir)
				m := download.New(cfg, client)
				m.Start()
				defer func() { m.Stop() }()
				synctest.Wait()
				if tc.create {
					data, err := os.ReadFile(filepath.Join(dir, ".plundrio-files/101.json"))
					if err != nil {
						t.Fatal(err)
					}
					var stored download.LocalManifest
					if err := json.Unmarshal(data, &stored); err != nil || stored.LocalRoot != root || stored.TransferID != 101 {
						t.Fatalf("new manifest: %s %v", data, err)
					}
					before = nameDriftSnapshot(t, dir)
					m.Stop()
					client.name = "new-root"
					m = download.New(cfg, client)
					m.Start()
					synctest.Wait()
				}
				state, ok := m.GetTransferContext(101)
				if !ok {
					t.Fatal("missing context")
				}
				if tc.wantErr != "" {
					if state.GetState() != download.TransferLifecycleFailed || state.GetError() == nil || !strings.Contains(state.GetError().Error(), tc.wantErr) {
						t.Fatalf("expected %s: %+v", tc.wantErr, state)
					}
				} else if state.GetState() != download.TransferLifecycleDownloading || state.Name != root {
					t.Fatalf("wrong retry destination: %+v", state)
				}
				nameDriftUnchanged(t, dir, before)
			})
		})
	}
}

func TestManifestNameDriftFileEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, payload                string
		pending, processed, complete bool
	}{
		{"complete", "book", true, true, true},
		{"partial", "bo", true, true, false},
		{"resized after import", "bookbook", false, true, false},
		{"missing after import", "", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeManifestFixture(t, dir, ".plundrio-files/101.json", []byte(`[{"name":"old-root/file.epub","length":4}]`))
			if err := os.MkdirAll(filepath.Join(dir, "old-root"), 0700); err != nil {
				t.Fatal(err)
			}
			if tc.payload != "" {
				writeManifestFixture(t, dir, "old-root/file.epub", []byte(tc.payload))
			}
			before := nameDriftSnapshot(t, dir)
			m := download.New(&config.Config{TargetDir: dir}, nil)
			for mode, want := range []bool{tc.pending, tc.processed, tc.complete} {
				_, err := m.GetTransferManifest(&putio.Transfer{ID: 101, Name: "new-root"}, download.ManifestCheck(mode))
				if (err == nil) != want {
					t.Fatalf("mode %d: %v, want accepted=%t", mode, err, want)
				}
			}
			nameDriftUnchanged(t, dir, before)
		})
	}
}
