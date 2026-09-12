//go:build unix

package reconcile

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/elsbrock/go-putio"
)

// Reusing an inode within one filesystem timestamp tick can reproduce the
// entire stat tuple. Freeze real Unix metadata so this case is deterministic
// on both high-resolution developer filesystems and the NAS filesystem.
type fixedMetadataFS struct {
	info     fs.FileInfo
	contents string
}

func (f fixedMetadataFS) Lstat(string) (fs.FileInfo, error) { return f.info, nil }
func (f fixedMetadataFS) ReadLink(string) (string, error)   { return "", fs.ErrInvalid }
func (f fixedMetadataFS) Open(string) (fs.File, error) {
	return fixedMetadataFile{strings.NewReader(f.contents), f.info}, nil
}

type fixedMetadataFile struct {
	*strings.Reader
	info fs.FileInfo
}

func (f fixedMetadataFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f fixedMetadataFile) Close() error               { return nil }

type fixedDirectoryFS struct {
	fixedMetadataFS
	directory fs.FileInfo
}

func (f fixedDirectoryFS) Lstat(name string) (fs.FileInfo, error) {
	if name == "folder" {
		return f.directory, nil
	}
	return f.info, nil
}

func (f fixedDirectoryFS) ReadDir(string) ([]fs.DirEntry, error) {
	return []fs.DirEntry{fs.FileInfoToDirEntry(f.info)}, nil
}

func TestLocalIdentityRejectsDifferentContentsWithIdenticalMetadata(t *testing.T) {
	path := t.TempDir() + "/payload"
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := confinedLocalNode(context.Background(), fixedMetadataFS{info, "old"}, "payload")
	if err != nil {
		t.Fatal(err)
	}
	after, err := confinedLocalNode(context.Background(), fixedMetadataFS{info, "new"}, "payload")
	if err != nil {
		t.Fatal(err)
	}
	if before.object.ID == after.object.ID {
		t.Fatal("different contents with identical metadata retained the selected deletion ID")
	}
	// The report and final deletion check must use exactly the same identity.
	for _, contents := range []string{"old", "new"} {
		node := &localNode{relPath: "payload", info: info}
		if err := fingerprintLocalTree(context.Background(), fixedMetadataFS{info, contents}, node); err != nil {
			t.Fatal(err)
		}
		want := before.object.ID
		if contents == "new" {
			want = after.object.ID
		}
		if node.object.ID != want {
			t.Fatal("report and deletion identity differ")
		}
		if node.object.ID == localID("payload", info, nil) {
			t.Fatal("legacy metadata-only ID retained")
		}
	}
	// A wholly unmanaged directory also changes when only a child's bytes do.
	directory, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, contents := range []string{"old", "new"} {
		node := &localNode{relPath: "folder", info: directory, children: []*localNode{{relPath: "folder/payload", info: info}}}
		if err := fingerprintLocalTree(context.Background(), fixedDirectoryFS{fixedMetadataFS{info, contents}, directory}, node); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, node.object.ID)
	}
	if ids[0] == ids[1] {
		t.Fatal("directory retained ID after child contents changed")
	}
}

func TestFingerprintRefusesFileReplacedByFIFO(t *testing.T) {
	path := t.TempDir()
	payload := filepath.Join(path, "payload")
	mustWrite(t, payload, "old")
	info, err := os.Stat(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(payload); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(payload, 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	done := make(chan struct{})
	var readErr error
	go func() {
		defer close(done)
		_, readErr = contentLocalID(context.Background(), localContentFS(root), &localNode{relPath: "payload", info: info})
	}()
	defer func() {
		// Release an incorrectly blocking implementation before TempDir cleanup.
		writer, err := os.OpenFile(payload, os.O_RDWR|syscall.O_NONBLOCK, 0)
		if err == nil {
			defer writer.Close()
		}
		<-done
	}()
	select {
	case <-done:
		if readErr == nil {
			t.Fatal("accepted FIFO replacement")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fingerprinting blocked opening replacement FIFO")
	}
}

func TestFingerprintRefusesDirectoryChangedAfterEnumeration(t *testing.T) {
	path := t.TempDir()
	mustMkdir(t, filepath.Join(path, "folder"))
	mustWrite(t, filepath.Join(path, "folder", "selected"), "old")
	nodes, err := buildLocalTree(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(path, "folder", "unselected"), "new")
	if err := fingerprintUnmanaged(context.Background(), path, nodes, nil); err == nil {
		t.Fatal("accepted stale directory listing containing an unselected child")
	}
}

func TestContentIdentityFailsClosedOnIncompleteRead(t *testing.T) {
	path := t.TempDir() + "/payload"
	mustWrite(t, path, "old")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, contents := range []string{"", "ol", "olds"} {
		if _, err := confinedLocalNode(context.Background(), fixedMetadataFS{info, contents}, "payload"); err == nil {
			t.Fatalf("accepted contents %q inconsistent with snapshot size", contents)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := contentLocalID(ctx, fixedMetadataFS{info, "old"}, &localNode{relPath: "payload", info: info}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}

func TestFingerprintSkipsActivePayloads(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "unmanaged"), "old")
	info, err := os.Stat(filepath.Join(root, "unmanaged"))
	if err != nil {
		t.Fatal(err)
	}
	// Nonexistent active payloads make any accidental attempt to open them fail.
	nodes := []*localNode{
		{relPath: "active", info: info},
		{relPath: "category", children: []*localNode{{relPath: filepath.Join("category", "active"), info: info}}},
		{relPath: "unmanaged", info: info},
	}
	active := map[string]struct{}{"active": {}, filepath.Join("category", "active"): {}}
	if err := fingerprintUnmanaged(context.Background(), root, nodes, active); err != nil {
		t.Fatal(err)
	}
	if nodes[0].object.ID != "" || nodes[1].children[0].object.ID != "" {
		t.Fatal("active payload was fingerprinted")
	}
	if nodes[2].object.ID == "" {
		t.Fatal("unmanaged payload was not fingerprinted")
	}
}

func TestLocalDeleteRechecksOwnershipAfterContentValidation(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "payload"), "old")
	client := &fakeClient{files: map[int64][]*putio.File{}}
	service := New(client, 1, root, false)
	id := unmanagedObjectID(t, service, "local", "payload")
	client.onTransfers = func(call int) {
		// Preview, batch inventory, selected snapshot, then the post-read check.
		if call == 4 {
			client.transfers = []*putio.Transfer{{ID: 12, Name: "payload", SaveParentID: 1}}
		}
	}
	report, err := service.Delete(context.Background(), DeleteOptions{IDs: []string{id}, Local: true, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.Deleted != 0 || report.Summary.Failed != 1 || !strings.Contains(report.Results[0].Error, "became active") {
		t.Fatalf("new ownership was not protected: %+v", report)
	}
	contents, err := os.ReadFile(filepath.Join(root, "payload"))
	if err != nil || string(contents) != "old" {
		t.Fatalf("active payload changed: %q, %v", contents, err)
	}
}
