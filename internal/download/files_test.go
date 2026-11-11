package download

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestTransferFileStorePersists(t *testing.T) {
	dir := t.TempDir()
	store := newTransferFileStore(dir)
	want := []TransferFile{{Name: "Book/book.m4b", Length: 42}}
	if err := store.Set(101, want); err != nil {
		t.Fatal(err)
	}

	got, ok := newTransferFileStore(dir).Get(101)
	if !ok {
		t.Fatal("persisted transfer file manifest was not loaded")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("manifest = %+v, want %+v", got, want)
	}
}

func TestTransferFileStoreRemovePersists(t *testing.T) {
	dir := t.TempDir()
	store := newTransferFileStore(dir)
	if err := store.Set(101, []TransferFile{{Name: "Book/book.m4b", Length: 42}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(101); err != nil {
		t.Fatal(err)
	}

	if _, ok := newTransferFileStore(dir).Get(101); ok {
		t.Fatal("removed transfer file manifest was restored")
	}
}

func TestTransferFileStoreRejectsEmptyManifest(t *testing.T) {
	store := newTransferFileStore(t.TempDir())
	if err := store.Set(101, nil); err == nil {
		t.Fatal("expected empty manifest to fail")
	}
	if _, err := os.Stat(store.path(101)); !os.IsNotExist(err) {
		t.Fatalf("empty manifest created state file: %v", err)
	}
}

func TestTransferFileStoreRewritePreservesOpenReaders(t *testing.T) {
	store := newTransferFileStore(t.TempDir())
	before := []TransferFile{{Name: "saved-root/old.epub", Length: 42}}
	after := []TransferFile{{Name: "saved-root/new.epub", Length: 7}}
	if err := store.Set(101, before); err != nil {
		t.Fatal(err)
	}
	// Pause a reader after opening the manifest, before consuming its bytes.
	reader, err := os.Open(store.path(101))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := store.Set(101, after); err != nil {
		t.Fatal(err)
	}
	var got []TransferFile
	if err := json.NewDecoder(reader).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, before) {
		t.Fatalf("open reader saw rewritten manifest: %+v, want %+v", got, before)
	}
	got, err = store.load(101)
	if err != nil || !reflect.DeepEqual(got, after) {
		t.Fatalf("new reader = %+v, %v, want %+v", got, err, after)
	}
	info, err := os.Stat(store.path(101))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("manifest permissions = %o, want 600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(store.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "101.json" {
		t.Fatalf("unexpected state files: %v", entries)
	}
}

func TestTransferFileStoreFailedPublishRemovesTemporaryFile(t *testing.T) {
	store := newTransferFileStore(t.TempDir())
	// A nonempty directory prevents replacement of the destination.
	if err := os.MkdirAll(store.path(101), 0700); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(store.path(101), "keep")
	if err := os.WriteFile(kept, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(101, []TransferFile{{Name: "saved-root/file.epub", Length: 7}}); err == nil {
		t.Fatal("expected publish failure")
	}
	if data, err := os.ReadFile(kept); err != nil || string(data) != "unchanged" {
		t.Fatalf("failed publish changed existing state: %q, %v", data, err)
	}
	entries, err := os.ReadDir(store.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "101.json" {
		t.Fatalf("failed publish left temporary files: %v", entries)
	}
}

func TestTransferRootUsesFirstPathElement(t *testing.T) {
	for _, tc := range []struct {
		remote string
		files  []TransferFile
		want   string
	}{
		{"remote", nil, "remote"},
		{"remote", []TransferFile{{Name: "Book/book.m4b"}}, "Book"},
		{"remote", []TransferFile{{Name: "Show/Subs/ep1/2_eng.srt"}, {Name: "Show/ep1.mkv"}}, "Show"},
		{"Show", []TransferFile{{Name: "Show/Subs/ep1/2_eng.srt"}}, "Show"},
		// Flat manifests saved before nested paths were kept may have a
		// multi-element root; the unrenamed remote name still identifies it.
		{"tv/Show", []TransferFile{{Name: "tv/Show/ep1.mkv"}}, "tv/Show"},
	} {
		if got := TransferRoot(tc.remote, tc.files); got != filepath.FromSlash(tc.want) {
			t.Errorf("TransferRoot(%q, %+v) = %q, want %q", tc.remote, tc.files, got, tc.want)
		}
	}
}
