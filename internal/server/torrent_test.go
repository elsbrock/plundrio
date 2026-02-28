package server

import (
	"os"
	"path/filepath"
	"testing"
)

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
			setup:        func(t *testing.T, targetDir string) {
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
