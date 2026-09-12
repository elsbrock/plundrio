package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/config"
	"github.com/elsbrock/plundrio/internal/download"
)

type torrentAddClient struct {
	addTransfer       *putio.Transfer
	uploadTransfer    *putio.Transfer
	transfers         []*putio.Transfer
	addMagnet         string
	uploadData        []byte
	uploadFilename    string
	folderID          int64
	deletedFiles      []int64
	deleted           []int64
	deleteTransferErr error
}

func (c *torrentAddClient) GetAccountInfo(context.Context) (*putio.AccountInfo, error) {
	return nil, nil
}

func (c *torrentAddClient) GetTransfers(context.Context) ([]*putio.Transfer, error) {
	return c.transfers, nil
}

func (c *torrentAddClient) UploadFile(
	_ context.Context,
	data []byte,
	filename string,
	folderID int64,
) (*putio.Transfer, error) {
	c.uploadData = append([]byte(nil), data...)
	c.uploadFilename = filename
	c.folderID = folderID
	return c.uploadTransfer, nil
}

func (c *torrentAddClient) AddTransfer(
	_ context.Context,
	magnetLink string,
	folderID int64,
) (*putio.Transfer, error) {
	c.addMagnet = magnetLink
	c.folderID = folderID
	return c.addTransfer, nil
}

func (c *torrentAddClient) EnsureFolderInParent(_ context.Context, _ string, parentID int64) (int64, error) {
	return parentID, nil
}

func (c *torrentAddClient) DeleteFile(_ context.Context, fileID int64) error {
	c.deletedFiles = append(c.deletedFiles, fileID)
	return nil
}

func (c *torrentAddClient) DeleteTransfer(_ context.Context, transferID int64) error {
	c.deleted = append(c.deleted, transferID)
	return c.deleteTransferErr
}

type torrentAddDownloadService struct {
	categories       map[int64]string
	files            map[int64][]download.TransferFile
	transfers        []*putio.Transfer
	removedTransfers []int64
	pending          map[int64]string
	contexts         map[int64]*download.TransferContext
}

func (s *torrentAddDownloadService) GetTransfers() []*putio.Transfer {
	return s.transfers
}

func (s *torrentAddDownloadService) GetTransferContext(transferID int64) (*download.TransferContext, bool) {
	ctx, ok := s.contexts[transferID]
	return ctx, ok
}

func (s *torrentAddDownloadService) GetTransferFiles(transferID int64) ([]download.TransferFile, bool) {
	files, ok := s.files[transferID]
	return files, ok
}

func (s *torrentAddDownloadService) SetCategory(transferID int64, category string) {
	if s.categories == nil {
		s.categories = make(map[int64]string)
	}
	s.categories[transferID] = category
}

func (s *torrentAddDownloadService) GetCategory(transferID int64) string {
	if category, ok := s.pending[transferID]; ok {
		return category
	}
	return s.categories[transferID]
}

func (s *torrentAddDownloadService) PrepareRemoval(id int64, _ bool) (string, error) {
	if s.pending == nil {
		s.pending = make(map[int64]string)
	}
	s.pending[id] = s.GetCategory(id)
	delete(s.categories, id)
	return s.pending[id], nil
}

func (s *torrentAddDownloadService) RemovalPending(id int64) bool {
	_, ok := s.pending[id]
	return ok
}

func (s *torrentAddDownloadService) NeedsReview(int64) bool { return false }
func (s *torrentAddDownloadService) PrepareReviewRetirement(context.Context, *putio.Transfer) (string, error) {
	return "", fmt.Errorf("not a reviewed record")
}

func (s *torrentAddDownloadService) RemoveCategory(transferID int64) {
	delete(s.categories, transferID)
}

func (s *torrentAddDownloadService) RemoveTransfer(transferID int64) {
	s.removedTransfers = append(s.removedTransfers, transferID)
	delete(s.files, transferID)
	delete(s.pending, transferID)
}

func TestHandleTorrentAddReturnsMagnetTrackingFields(t *testing.T) {
	client := &torrentAddClient{
		addTransfer: &putio.Transfer{ID: 7, Hash: "ABC123", Name: "Example Show"},
	}
	service := &torrentAddDownloadService{}
	server := &Server{
		cfg:       &config.Config{FolderID: 42, TargetDir: "/downloads", UseCategoriesTarget: true},
		client:    client,
		dlService: service,
	}
	magnet := "magnet:?xt=urn:btih:ABC123"
	args, err := json.Marshal(map[string]string{
		"magnetLink":   magnet,
		"download-dir": "/downloads/tv",
	})
	if err != nil {
		t.Fatal(err)
	}

	response, err := server.handleTorrentAdd(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}

	assertTorrentAddedResponse(t, response, 7, "ABC123", "Example Show")
	if client.addMagnet != magnet || client.folderID != 42 {
		t.Fatalf("AddTransfer called with magnet %q and folder %d", client.addMagnet, client.folderID)
	}
	if got := service.categories[7]; got != "tv" {
		t.Fatalf("stored category = %q, want tv", got)
	}
}

func TestHandleTorrentAddReturnsMetainfoTrackingFields(t *testing.T) {
	client := &torrentAddClient{
		uploadTransfer: &putio.Transfer{ID: 8, Hash: "DEF456", Name: "Example Movie"},
	}
	service := &torrentAddDownloadService{}
	server := &Server{
		cfg:       &config.Config{FolderID: 42, TargetDir: "/downloads"},
		client:    client,
		dlService: service,
	}
	torrentData := []byte("torrent-data")
	args, err := json.Marshal(map[string]string{
		"filename": "Example.torrent",
		"metainfo": base64.StdEncoding.EncodeToString(torrentData),
	})
	if err != nil {
		t.Fatal(err)
	}

	response, err := server.handleTorrentAdd(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}

	assertTorrentAddedResponse(t, response, 8, "DEF456", "Example Movie")
	if string(client.uploadData) != string(torrentData) ||
		client.uploadFilename != "Example.torrent" ||
		client.folderID != 42 {
		t.Fatalf(
			"UploadFile called with data %q, filename %q, folder %d",
			client.uploadData,
			client.uploadFilename,
			client.folderID,
		)
	}
}

// A magnet whose info-hash put.io has not resolved yet must still succeed, and
// its category must still be recorded: categories are keyed by transfer ID,
// which put.io always populates, rather than by the hash.
func TestHandleTorrentAddAllowsEmptyHash(t *testing.T) {
	client := &torrentAddClient{
		addTransfer: &putio.Transfer{ID: 7, Name: "Example Show"},
	}
	service := &torrentAddDownloadService{}
	server := &Server{
		cfg:       &config.Config{FolderID: 42, TargetDir: "/downloads", UseCategoriesTarget: true},
		client:    client,
		dlService: service,
	}
	args, err := json.Marshal(map[string]string{
		"magnetLink":   "magnet:?xt=urn:btih:ABC123",
		"download-dir": "/downloads/tv",
	})
	if err != nil {
		t.Fatal(err)
	}

	response, err := server.handleTorrentAdd(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}

	assertTorrentAddedResponse(t, response, 7, "", "Example Show")
	if got := service.categories[7]; got != "tv" {
		t.Fatalf("stored category = %q, want tv", got)
	}
}

func TestHandleTorrentAddRejectsMissingTransferID(t *testing.T) {
	client := &torrentAddClient{
		addTransfer: &putio.Transfer{Hash: "ABC123", Name: "Example Show"},
	}
	server := &Server{
		cfg:       &config.Config{FolderID: 42, TargetDir: "/downloads"},
		client:    client,
		dlService: &torrentAddDownloadService{},
	}
	args, err := json.Marshal(map[string]string{
		"magnetLink": "magnet:?xt=urn:btih:ABC123",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := server.handleTorrentAdd(context.Background(), args); err == nil {
		t.Fatal("expected missing transfer ID to fail torrent-add")
	}
}

func assertTorrentAddedResponse(
	t *testing.T,
	response interface{},
	wantID int64,
	wantHash string,
	wantName string,
) {
	t.Helper()

	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}

	var decoded struct {
		TorrentAdded struct {
			ID         int64  `json:"id"`
			HashString string `json:"hashString"`
			Name       string `json:"name"`
		} `json:"torrent-added"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.TorrentAdded.ID != wantID ||
		decoded.TorrentAdded.HashString != wantHash ||
		decoded.TorrentAdded.Name != wantName {
		t.Fatalf("unexpected torrent-added response: %s", body)
	}
}

type torrentGetRPCResponse struct {
	Result    string `json:"result"`
	Arguments struct {
		Torrents []torrentGetRPCTorrent `json:"torrents"`
	} `json:"arguments"`
}

type torrentGetRPCTorrent struct {
	HashString     string  `json:"hashString"`
	Status         int     `json:"status"`
	LeftUntilDone  int64   `json:"leftUntilDone"`
	SeedRatioMode  int     `json:"seedRatioMode"`
	SeedRatioLimit float64 `json:"seedRatioLimit"`
	SeedIdleMode   int     `json:"seedIdleMode"`
	SeedIdleLimit  int64   `json:"seedIdleLimit"`
	SecondsSeeding int64   `json:"secondsSeeding"`
}

func TestTorrentGetReportsCompletedTransfersAsRemovable(t *testing.T) {
	completedCtx := download.NewTransferContext(1, 1, download.TransferLifecycleProcessed)
	completedCtx.SetTotalSize(1000)
	completedCtx.AddDownloadedBytes(1000)

	copyingCtx := download.NewTransferContext(2, 1, download.TransferLifecycleDownloading)
	copyingCtx.SetTotalSize(1000)
	copyingCtx.AddDownloadedBytes(500)

	downloadService := &torrentAddDownloadService{
		transfers: []*putio.Transfer{
			{ID: 1, Hash: "completed", Name: "completed.mkv", PercentDone: 100, Status: "COMPLETED", Size: 1000},
			{ID: 2, Hash: "copying", Name: "copying.mkv", PercentDone: 100, Status: "COMPLETED", Size: 1000},
			{ID: 3, Hash: "error", Name: "error.mkv", PercentDone: 30, Status: "ERROR", Size: 1000},
		},
		contexts: map[int64]*download.TransferContext{
			1: completedCtx,
			2: copyingCtx,
		},
	}

	server := &Server{
		cfg:       &config.Config{TargetDir: "/downloads"},
		dlService: downloadService,
	}
	requestBody := []byte(`{"method":"torrent-get","arguments":{"fields":["hashString","status","leftUntilDone","secondsSeeding","seedRatioLimit","seedRatioMode","seedIdleLimit","seedIdleMode"]}}`)
	request := httptest.NewRequest(http.MethodPost, "/transmission/rpc", bytes.NewReader(requestBody))
	request.Header.Set("X-Transmission-Session-Id", "123")
	responseRecorder := httptest.NewRecorder()

	server.handleRPC(responseRecorder, request)

	if responseRecorder.Code != http.StatusOK {
		t.Fatalf("torrent-get status = %d, want %d", responseRecorder.Code, http.StatusOK)
	}

	var response torrentGetRPCResponse
	if err := json.NewDecoder(responseRecorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode torrent-get response: %v", err)
	}
	if response.Result != "success" {
		t.Fatalf("torrent-get result = %q, want success", response.Result)
	}

	torrentsByHash := make(map[string]torrentGetRPCTorrent, len(response.Arguments.Torrents))
	for _, torrent := range response.Arguments.Torrents {
		torrentsByHash[torrent.HashString] = torrent
	}

	tests := []struct {
		hash           string
		status         int
		leftUntilDone  int64
		seedIdleMode   int
		secondsSeeding int64
	}{
		{hash: "completed", status: trStatusSeed, seedIdleMode: 1, secondsSeeding: 1},
		{hash: "copying", status: trStatusDownload, leftUntilDone: 500, seedIdleMode: 2},
		{hash: "error", status: trStatusStopped, leftUntilDone: 700, seedIdleMode: 2},
	}

	for _, tt := range tests {
		got, ok := torrentsByHash[tt.hash]
		if !ok {
			t.Errorf("torrent %q missing from response", tt.hash)
			continue
		}
		if got.Status != tt.status ||
			got.LeftUntilDone != tt.leftUntilDone ||
			got.SeedRatioMode != 2 ||
			got.SeedRatioLimit != 0 ||
			got.SeedIdleMode != tt.seedIdleMode ||
			got.SeedIdleLimit != 0 ||
			got.SecondsSeeding != tt.secondsSeeding {
			t.Errorf("torrent %q has unexpected RPC fields: %+v", tt.hash, got)
		}
	}
}

func TestTorrentGetRequiresProcessedLifecycleForRemovability(t *testing.T) {
	for _, state := range []download.TransferLifecycleState{
		download.TransferLifecycleInitial,
		download.TransferLifecycleCompleted,
	} {
		t.Run(fmt.Sprint(state), func(t *testing.T) {
			ctx := download.NewTransferContext(1, 1, state)
			ctx.SetTotalSize(1000)
			ctx.AddDownloadedBytes(1000)
			assertNotRemovable(t, ctx)
		})
	}
	t.Run("no local context", func(t *testing.T) { assertNotRemovable(t, nil) })
}

func assertNotRemovable(t *testing.T, ctx *download.TransferContext) {
	t.Helper()
	service := &torrentAddDownloadService{
		transfers: []*putio.Transfer{{ID: 1, Hash: "pending", Status: "COMPLETED", PercentDone: 100, Size: 1000}},
		contexts:  map[int64]*download.TransferContext{},
	}
	if ctx != nil {
		service.contexts[1] = ctx
	}
	server := &Server{cfg: &config.Config{TargetDir: t.TempDir()}, dlService: service}
	request := httptest.NewRequest(http.MethodPost, "/transmission/rpc", bytes.NewBufferString(`{"method":"torrent-get","arguments":{"fields":["seedIdleMode","secondsSeeding"]}}`))
	request.Header.Set("X-Transmission-Session-Id", "123")
	recorder := httptest.NewRecorder()
	server.handleRPC(recorder, request)
	var response torrentGetRPCResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || response.Result != "success" || len(response.Arguments.Torrents) != 1 {
		t.Fatalf("unexpected RPC response: %d %s", recorder.Code, recorder.Body.String())
	}
	got := response.Arguments.Torrents[0]
	if got.SeedIdleMode != transmissionLimitModeUnlimited || got.SecondsSeeding != 0 {
		t.Fatalf("unprocessed transfer marked removable: %+v", got)
	}
}

func TestDeleteLocalData(t *testing.T) {
	tests := []struct {
		name         string
		transferName string
		setup        func(t *testing.T, targetDir string)
		wantErr      bool
		wantDeleted  bool
	}{
		{
			name:         "deletes transfer directory",
			transferName: "My.Show.S01E01",
			setup: func(t *testing.T, targetDir string) {
				dir := filepath.Join(targetDir, "My.Show.S01E01")
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "episode.mkv"), []byte("data"), 0644); err != nil {
					t.Fatal(err)
				}
			},
			wantDeleted: true,
		},
		{
			name:         "deletes single file transfer",
			transferName: "movie.mkv",
			setup: func(t *testing.T, targetDir string) {
				if err := os.WriteFile(filepath.Join(targetDir, "movie.mkv"), []byte("data"), 0644); err != nil {
					t.Fatal(err)
				}
			},
			wantDeleted: true,
		},
		{
			name:         "no error when path does not exist",
			transferName: "nonexistent",
			setup:        func(t *testing.T, targetDir string) {},
			wantDeleted:  false,
		},
		{
			name:         "rejects path traversal with ..",
			transferName: "../../etc/passwd",
			setup:        func(t *testing.T, targetDir string) {},
			wantErr:      true,
		},
		{
			name:         "absolute path in transfer name is safe",
			transferName: "/tmp/evil",
			setup: func(t *testing.T, targetDir string) {
				// filepath.Join strips leading / so this resolves inside targetDir
			},
			wantDeleted: false,
		},
		{
			name:         "deletes nested directory structure",
			transferName: "Show.S01",
			setup: func(t *testing.T, targetDir string) {
				dir := filepath.Join(targetDir, "Show.S01", "Subs")
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "english.srt"), []byte("subs"), 0644); err != nil {
					t.Fatal(err)
				}
				parent := filepath.Join(targetDir, "Show.S01")
				if err := os.WriteFile(filepath.Join(parent, "episode.mkv"), []byte("video"), 0644); err != nil {
					t.Fatal(err)
				}
			},
			wantDeleted: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			targetDir := t.TempDir()
			tt.setup(t, targetDir)

			err := deleteLocalData(targetDir, tt.transferName)

			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if tt.wantDeleted {
				localPath := filepath.Join(targetDir, tt.transferName)
				if _, err := os.Stat(localPath); !os.IsNotExist(err) {
					t.Errorf("expected %q to be deleted, but it still exists", localPath)
				}
			}
		})
	}
}

func TestExtractCategory(t *testing.T) {
	tests := []struct {
		name        string
		targetDir   string
		downloadDir string
		want        string
	}{
		{
			name:        "with category",
			targetDir:   "/downloads",
			downloadDir: "/downloads/tv",
			want:        "tv",
		},
		{
			name:        "empty downloadDir",
			targetDir:   "/downloads",
			downloadDir: "",
			want:        "",
		},
		{
			name:        "same as targetDir",
			targetDir:   "/downloads",
			downloadDir: "/downloads",
			want:        "",
		},
		{
			name:        "nested category",
			targetDir:   "/downloads",
			downloadDir: "/downloads/media/tv",
			want:        "media/tv",
		},
		{
			name:        "escaping downloadDir is rejected",
			targetDir:   "/downloads",
			downloadDir: "/etc",
			want:        "",
		},
		{
			name:        "parent of targetDir is rejected",
			targetDir:   "/downloads/complete",
			downloadDir: "/downloads",
			want:        "",
		},
		{
			name:        "sibling escaping targetDir is rejected",
			targetDir:   "/downloads",
			downloadDir: "/downloads2/tv",
			want:        "",
		},
		{
			name:        "trailing slash on downloadDir",
			targetDir:   "/downloads",
			downloadDir: "/downloads/tv/",
			want:        "tv",
		},
		{
			name:        "trailing slash on targetDir",
			targetDir:   "/downloads/",
			downloadDir: "/downloads/tv",
			want:        "tv",
		},
		{
			name:        "both trailing slashes",
			targetDir:   "/downloads/",
			downloadDir: "/downloads/tv/",
			want:        "tv",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractCategory(tt.targetDir, tt.downloadDir)
			if got != tt.want {
				t.Errorf("extractCategory(%q, %q) = %q, want %q", tt.targetDir, tt.downloadDir, got, tt.want)
			}
		})
	}
}

func TestDeleteLocalDataDoesNotAffectSiblings(t *testing.T) {
	targetDir := t.TempDir()

	// Create two transfer directories
	for _, name := range []string{"transfer-a", "transfer-b"} {
		dir := filepath.Join(targetDir, name)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "file.mkv"), []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Delete only transfer-a
	if err := deleteLocalData(targetDir, "transfer-a"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// transfer-a should be gone
	if _, err := os.Stat(filepath.Join(targetDir, "transfer-a")); !os.IsNotExist(err) {
		t.Error("transfer-a should have been deleted")
	}

	// transfer-b should still exist
	if _, err := os.Stat(filepath.Join(targetDir, "transfer-b", "file.mkv")); err != nil {
		t.Error("transfer-b should not have been affected")
	}
}
