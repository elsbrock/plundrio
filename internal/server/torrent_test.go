package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/config"
	"github.com/elsbrock/plundrio/internal/download"
)

type torrentAddClient struct {
	addTransfer    *putio.Transfer
	uploadTransfer *putio.Transfer
	addMagnet      string
	uploadData     []byte
	uploadFilename string
	folderID       int64
}

func (c *torrentAddClient) GetAccountInfo(context.Context) (*putio.AccountInfo, error) {
	return nil, nil
}

func (c *torrentAddClient) GetTransfers(context.Context) ([]*putio.Transfer, error) {
	return nil, nil
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

func (c *torrentAddClient) DeleteFile(context.Context, int64) error {
	return nil
}

func (c *torrentAddClient) DeleteTransfer(context.Context, int64) error {
	return nil
}

type torrentAddDownloadService struct {
	categories map[string]string
}

func (s *torrentAddDownloadService) GetTransfers() []*putio.Transfer {
	return nil
}

func (s *torrentAddDownloadService) GetTransferContext(int64) (*download.TransferContext, bool) {
	return nil, false
}

func (s *torrentAddDownloadService) SetCategory(hash, category string) {
	if s.categories == nil {
		s.categories = make(map[string]string)
	}
	s.categories[hash] = category
}

func (s *torrentAddDownloadService) GetCategory(hash string) string {
	return s.categories[hash]
}

func (s *torrentAddDownloadService) RemoveCategory(hash string) {
	delete(s.categories, hash)
}

func (s *torrentAddDownloadService) Stop() {}

func TestHandleTorrentAddReturnsMagnetTrackingFields(t *testing.T) {
	client := &torrentAddClient{
		addTransfer: &putio.Transfer{ID: 7, Hash: "ABC123", Name: "Example Show"},
	}
	service := &torrentAddDownloadService{}
	server := &Server{
		cfg:       &config.Config{FolderID: 42, TargetDir: "/downloads"},
		client:    client,
		dlService: service,
	}
	magnet := "magnet:?xt=urn:btih:ABC123"
	args, err := json.Marshal(map[string]string{
		"magnetLink":  magnet,
		"downloadDir": "/downloads/tv",
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
	if got := service.categories["ABC123"]; got != "tv" {
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

func TestHandleTorrentAddAllowsEmptyHashWithoutCategory(t *testing.T) {
	client := &torrentAddClient{
		addTransfer: &putio.Transfer{ID: 7, Name: "Example Show"},
	}
	service := &torrentAddDownloadService{}
	server := &Server{
		cfg:       &config.Config{FolderID: 42, TargetDir: "/downloads"},
		client:    client,
		dlService: service,
	}
	args, err := json.Marshal(map[string]string{
		"magnetLink":  "magnet:?xt=urn:btih:ABC123",
		"downloadDir": "/downloads/tv",
	})
	if err != nil {
		t.Fatal(err)
	}

	response, err := server.handleTorrentAdd(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}

	assertTorrentAddedResponse(t, response, 7, "", "Example Show")
	if len(service.categories) != 0 {
		t.Fatalf("stored categories = %#v, want none", service.categories)
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
