package download

import (
	"fmt"
	"os"
	"testing"
)

// A concurrent reader (another process, or this one after a crash) must never
// observe a half-written ownership record: a truncated manifest parses as
// corrupt and fails every transfer closed.
func TestTransferFileStoreWriteIsAtomic(t *testing.T) {
	dir := t.TempDir()
	files := make([]TransferFile, 0, 2000)
	for i := range cap(files) {
		files = append(files, TransferFile{Name: fmt.Sprintf("Show/S01/episode-%04d.mkv", i), Length: int64(i + 1)})
	}
	manifest := LocalManifest{TransferID: 101, LocalRoot: "Show", Files: files}
	writer, reader := newTransferFileStore(dir), newTransferFileStore(dir)
	if err := writer.setManifest(manifest); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	reads := make(chan error, 1)
	go func() {
		var failure error
		observed := 0
		for {
			got, err := reader.loadManifest(101)
			if err != nil {
				failure = err
			} else if got.TransferID != 101 || got.LocalRoot != "Show" {
				failure = fmt.Errorf("lost manifest identity: %+v", got)
			} else if len(got.Files) != len(files) {
				failure = fmt.Errorf("partial manifest: %d of %d files", len(got.Files), len(files))
			} else {
				observed++
			}
			select {
			case <-done:
				if observed == 0 && failure == nil {
					failure = fmt.Errorf("reader never observed the manifest")
				}
				reads <- failure
				return
			default:
			}
		}
	}()
	for i := 0; i < 50; i++ {
		files[0].Length = int64(i + 1)
		if err := writer.setManifest(manifest); err != nil {
			t.Fatal(err)
		}
	}
	close(done)
	if err := <-reads; err != nil {
		t.Fatalf("concurrent read of a rewritten manifest failed: %v", err)
	}
	if entries, err := os.ReadDir(newTransferFileStore(dir).stateDir); err != nil || len(entries) != 1 {
		t.Fatalf("publication left temporary state behind: %+v %v", entries, err)
	}
}
