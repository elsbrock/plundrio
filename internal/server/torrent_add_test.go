package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/config"
	"github.com/elsbrock/plundrio/internal/download"
)

// mockPutioClient records the put.io calls made by the RPC server.
type mockPutioClient struct {
	addTransferFolderID int64
	uploadFolderID      int64
	ensureCalls         []ensureCall

	nextTransferID   int64
	nextTransferHash string
	ensureResult     int64
	ensureErr        error
}

type ensureCall struct {
	name   string
	parent int64
}

func (m *mockPutioClient) GetAccountInfo(ctx context.Context) (*putio.AccountInfo, error) {
	return &putio.AccountInfo{}, nil
}

func (m *mockPutioClient) GetTransfers(ctx context.Context) ([]*putio.Transfer, error) {
	return nil, nil
}

func (m *mockPutioClient) UploadFile(ctx context.Context, data []byte, filename string, folderID int64) (*putio.Transfer, error) {
	m.uploadFolderID = folderID
	return &putio.Transfer{ID: m.nextTransferID, Hash: m.nextTransferHash}, nil
}

func (m *mockPutioClient) AddTransfer(ctx context.Context, magnetLink string, folderID int64) (*putio.Transfer, error) {
	m.addTransferFolderID = folderID
	return &putio.Transfer{ID: m.nextTransferID, Hash: m.nextTransferHash}, nil
}

func (m *mockPutioClient) EnsureFolderInParent(ctx context.Context, name string, parentID int64) (int64, error) {
	m.ensureCalls = append(m.ensureCalls, ensureCall{name: name, parent: parentID})
	if m.ensureErr != nil {
		return 0, m.ensureErr
	}
	return m.ensureResult, nil
}

func (m *mockPutioClient) DeleteFile(ctx context.Context, fileID int64) error { return nil }

func (m *mockPutioClient) DeleteTransfer(ctx context.Context, transferID int64) error { return nil }

// mockDownloadService records category bookkeeping made by the RPC server.
type mockDownloadService struct {
	categories map[int64]string
}

func newMockDownloadService() *mockDownloadService {
	return &mockDownloadService{categories: make(map[int64]string)}
}

func (d *mockDownloadService) GetTransfers() []*putio.Transfer { return nil }

func (d *mockDownloadService) GetTransferContext(transferID int64) (*download.TransferContext, bool) {
	return nil, false
}

func (d *mockDownloadService) SetCategory(transferID int64, category string) {
	d.categories[transferID] = category
}

func (d *mockDownloadService) GetCategory(transferID int64) string { return d.categories[transferID] }

func (d *mockDownloadService) RemoveCategory(transferID int64) { delete(d.categories, transferID) }

func (d *mockDownloadService) Stop() {}

func newTestServer(cfg *config.Config, client PutioClient, dl DownloadService) *Server {
	return &Server{
		cfg:        cfg,
		client:     client,
		dlService:  dl,
		catFolders: make(map[string]int64),
	}
}

func TestHandleTorrentAdd_MagnetCategoryStoredFromDownloadDir(t *testing.T) {
	cfg := &config.Config{TargetDir: "/downloads", FolderID: 100, UseCategoriesTarget: true}
	client := &mockPutioClient{nextTransferID: 555}
	dl := newMockDownloadService()
	s := newTestServer(cfg, client, dl)

	args := json.RawMessage(`{"filename":"magnet:?xt=urn:btih:abc","download-dir":"/downloads/tv"}`)
	if _, err := s.handleTorrentAdd(context.Background(), args); err != nil {
		t.Fatalf("handleTorrentAdd returned error: %v", err)
	}

	// The transfer goes to the configured folder (put.io categories disabled).
	if client.addTransferFolderID != 100 {
		t.Errorf("AddTransfer folderID = %d, want 100", client.addTransferFolderID)
	}
	if len(client.ensureCalls) != 0 {
		t.Errorf("EnsureFolderInParent should not be called when put.io categories disabled, got %v", client.ensureCalls)
	}
	// Category is keyed by the put.io transfer ID, not the (possibly empty) hash.
	if got := dl.GetCategory(555); got != "tv" {
		t.Errorf("stored category for transfer 555 = %q, want %q", got, "tv")
	}
}

func TestHandleTorrentAdd_CamelCaseDownloadDirIgnored(t *testing.T) {
	// Regression guard for #39: the legacy camelCase "downloadDir" must NOT be
	// interpreted as a category; only Transmission's "download-dir" is honored.
	cfg := &config.Config{TargetDir: "/downloads", FolderID: 100, UseCategoriesTarget: true}
	client := &mockPutioClient{nextTransferID: 555}
	dl := newMockDownloadService()
	s := newTestServer(cfg, client, dl)

	args := json.RawMessage(`{"filename":"magnet:?xt=urn:btih:abc","downloadDir":"/downloads/tv"}`)
	if _, err := s.handleTorrentAdd(context.Background(), args); err != nil {
		t.Fatalf("handleTorrentAdd returned error: %v", err)
	}

	if got := dl.GetCategory(555); got != "" {
		t.Errorf("camelCase downloadDir should be ignored, but stored category = %q", got)
	}
}

func TestHandleTorrentAdd_CategoryNotStoredWhenTargetDisabled(t *testing.T) {
	cfg := &config.Config{TargetDir: "/downloads", FolderID: 100, UseCategoriesTarget: false}
	client := &mockPutioClient{nextTransferID: 555}
	dl := newMockDownloadService()
	s := newTestServer(cfg, client, dl)

	args := json.RawMessage(`{"filename":"magnet:?xt=urn:btih:abc","download-dir":"/downloads/tv"}`)
	if _, err := s.handleTorrentAdd(context.Background(), args); err != nil {
		t.Fatalf("handleTorrentAdd returned error: %v", err)
	}

	if got := dl.GetCategory(555); got != "" {
		t.Errorf("category should not be stored when target categories disabled, got %q", got)
	}
}

func TestHandleTorrentAdd_PutioSubfolderCreated(t *testing.T) {
	cfg := &config.Config{TargetDir: "/downloads", FolderID: 100, UseCategoriesPutio: true}
	client := &mockPutioClient{nextTransferID: 555, ensureResult: 200}
	dl := newMockDownloadService()
	s := newTestServer(cfg, client, dl)

	args := json.RawMessage(`{"filename":"magnet:?xt=urn:btih:abc","download-dir":"/downloads/tv"}`)
	if _, err := s.handleTorrentAdd(context.Background(), args); err != nil {
		t.Fatalf("handleTorrentAdd returned error: %v", err)
	}

	if len(client.ensureCalls) != 1 || client.ensureCalls[0] != (ensureCall{name: "tv", parent: 100}) {
		t.Fatalf("EnsureFolderInParent calls = %v, want one call for (tv, 100)", client.ensureCalls)
	}
	if client.addTransferFolderID != 200 {
		t.Errorf("AddTransfer folderID = %d, want 200 (the category subfolder)", client.addTransferFolderID)
	}
}

func TestHandleTorrentAdd_PutioNestedSubfolderCreated(t *testing.T) {
	cfg := &config.Config{TargetDir: "/downloads", FolderID: 100, UseCategoriesPutio: true}
	// EnsureFolderInParent returns a distinct id per call so we can assert the walk.
	client := &mockPutioClient{nextTransferID: 555, ensureResult: 300}
	dl := newMockDownloadService()
	s := newTestServer(cfg, client, dl)

	args := json.RawMessage(`{"filename":"magnet:?xt=urn:btih:abc","download-dir":"/downloads/media/tv"}`)
	if _, err := s.handleTorrentAdd(context.Background(), args); err != nil {
		t.Fatalf("handleTorrentAdd returned error: %v", err)
	}

	if len(client.ensureCalls) != 2 {
		t.Fatalf("EnsureFolderInParent calls = %v, want 2 (media, then tv)", client.ensureCalls)
	}
	if client.ensureCalls[0] != (ensureCall{name: "media", parent: 100}) {
		t.Errorf("first ensure = %v, want (media, 100)", client.ensureCalls[0])
	}
	if client.ensureCalls[1] != (ensureCall{name: "tv", parent: 300}) {
		t.Errorf("second ensure = %v, want (tv, 300)", client.ensureCalls[1])
	}
}

func TestHandleTorrentAdd_PutioSubfolderCachedAcrossCalls(t *testing.T) {
	cfg := &config.Config{TargetDir: "/downloads", FolderID: 100, UseCategoriesPutio: true}
	client := &mockPutioClient{nextTransferID: 555, ensureResult: 200}
	dl := newMockDownloadService()
	s := newTestServer(cfg, client, dl)

	args := json.RawMessage(`{"filename":"magnet:?xt=urn:btih:abc","download-dir":"/downloads/tv"}`)
	for i := 0; i < 3; i++ {
		if _, err := s.handleTorrentAdd(context.Background(), args); err != nil {
			t.Fatalf("handleTorrentAdd call %d returned error: %v", i, err)
		}
	}

	if len(client.ensureCalls) != 1 {
		t.Errorf("EnsureFolderInParent called %d times, want 1 (cached thereafter)", len(client.ensureCalls))
	}
}

func TestHandleTorrentAdd_NoCategoryUsesConfiguredFolder(t *testing.T) {
	cfg := &config.Config{TargetDir: "/downloads", FolderID: 100, UseCategoriesPutio: true, UseCategoriesTarget: true}
	client := &mockPutioClient{nextTransferID: 555, ensureResult: 200}
	dl := newMockDownloadService()
	s := newTestServer(cfg, client, dl)

	// No download-dir → no category → configured folder, no subfolder creation.
	args := json.RawMessage(`{"filename":"magnet:?xt=urn:btih:abc"}`)
	if _, err := s.handleTorrentAdd(context.Background(), args); err != nil {
		t.Fatalf("handleTorrentAdd returned error: %v", err)
	}

	if len(client.ensureCalls) != 0 {
		t.Errorf("EnsureFolderInParent should not be called without a category, got %v", client.ensureCalls)
	}
	if client.addTransferFolderID != 100 {
		t.Errorf("AddTransfer folderID = %d, want 100", client.addTransferFolderID)
	}
	if got := dl.GetCategory(555); got != "" {
		t.Errorf("no category expected, got %q", got)
	}
}
