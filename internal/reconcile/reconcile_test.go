package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/download"
)

type fakeClient struct {
	transfers      []*putio.Transfer
	transferStates [][]*putio.Transfer
	transferCalls  int
	files          map[int64][]*putio.File
	deleteErrors   map[int64]error
	deleted        []int64
	fileCalls      map[int64]int
	onTransfers    func(int)
	onDelete       func(int64)
}

func (f *fakeClient) GetTransfers(context.Context) ([]*putio.Transfer, error) {
	index := f.transferCalls
	f.transferCalls++
	if f.onTransfers != nil {
		f.onTransfers(f.transferCalls)
	}
	if len(f.transferStates) != 0 {
		if index >= len(f.transferStates) {
			index = len(f.transferStates) - 1
		}
		return f.transferStates[index], nil
	}
	return f.transfers, nil
}

func (f *fakeClient) GetFiles(_ context.Context, folderID int64) ([]*putio.File, error) {
	if f.fileCalls == nil {
		f.fileCalls = make(map[int64]int)
	}
	f.fileCalls[folderID]++
	return f.files[folderID], nil
}

func (f *fakeClient) DeleteFile(_ context.Context, fileID int64) error {
	if err := f.deleteErrors[fileID]; err != nil {
		return err
	}
	f.deleted = append(f.deleted, fileID)
	if f.onDelete != nil {
		f.onDelete(fileID)
	}
	return nil
}

func TestReconcileReportsOnlyUnownedObjectsAsUnmanaged(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "tv", "Active.Show"))
	mustWrite(t, filepath.Join(root, "tv", "Active.Show", "episode.mkv"), "active")
	mustWrite(t, filepath.Join(root, "tv", "leftover.mkv"), "leftover")
	mustWrite(t, filepath.Join(root, download.CategoryStateFileName), `{"10":"tv"}`)

	client := &fakeClient{
		transfers: []*putio.Transfer{{
			ID: 10, FileID: 100, Hash: "active-hash", Name: "Active.Show", SaveParentID: 1,
		}},
		files: map[int64][]*putio.File{
			1: {
				putioFile(100, "Active.Show", 1, true, 6),
				putioFile(200, "leftover.mkv", 1, false, 8),
			},
			100: {putioFile(101, "episode.mkv", 100, false, 6)},
		},
	}

	report, err := New(client, 1, root, true).Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	wantActive := []string{"local:tv/Active.Show", "putio:Active.Show"}
	wantUnmanaged := []string{"local:tv/leftover.mkv", "putio:leftover.mkv"}
	if got := objectLabels(report.Active); !reflect.DeepEqual(got, wantActive) {
		t.Fatalf("active objects = %v, want %v", got, wantActive)
	}
	if got := objectLabels(report.Unmanaged); !reflect.DeepEqual(got, wantUnmanaged) {
		t.Fatalf("unmanaged objects = %v, want %v", got, wantUnmanaged)
	}
	if report.Summary.UnmanagedCount != 2 || report.Summary.UnmanagedBytes != 16 {
		t.Fatalf("unexpected unmanaged summary: %+v", report.Summary)
	}
}

func TestReconcileHandlesNestedAndEmptyRootsDeterministically(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "category", "active"))
	mustWrite(t, filepath.Join(root, "category", "active", "file"), "123")
	mustWrite(t, filepath.Join(root, "category", "z-unmanaged"), "12")
	mustWrite(t, filepath.Join(root, "category", "a-unmanaged"), "1")
	mustWrite(t, filepath.Join(root, download.CategoryStateFileName), `{"1":"category"}`)

	client := &fakeClient{
		transfers: []*putio.Transfer{{ID: 1, FileID: 11, Hash: "nested-hash", Name: "active", SaveParentID: 7}},
		files: map[int64][]*putio.File{
			7:  {putioFile(10, "container", 7, true, 0)},
			10: {putioFile(13, "z-unmanaged", 10, false, 2), putioFile(11, "active", 10, true, 3), putioFile(12, "a-unmanaged", 10, false, 1)},
			11: {putioFile(14, "file", 11, false, 3)},
		},
	}

	service := New(client, 7, root, true)
	first, err := service.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("unchanged reports differ:\n%s\n%s", firstJSON, secondJSON)
	}

	want := []string{
		"local:category/a-unmanaged",
		"local:category/z-unmanaged",
		"putio:container/a-unmanaged",
		"putio:container/z-unmanaged",
	}
	if got := objectLabels(first.Unmanaged); !reflect.DeepEqual(got, want) {
		t.Fatalf("unmanaged objects = %v, want %v", got, want)
	}

	empty, err := New(&fakeClient{files: map[int64][]*putio.File{}}, 9, t.TempDir(), true).Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if empty.Active == nil || empty.Unmanaged == nil || len(empty.Active) != 0 || len(empty.Unmanaged) != 0 {
		t.Fatalf("empty report should contain empty arrays: %+v", empty)
	}
}

func TestReconcileRejectsUnsafeActiveLocalPath(t *testing.T) {
	root := t.TempDir()
	client := &fakeClient{
		transfers: []*putio.Transfer{{ID: 1, Name: "../../../outside", SaveParentID: 1}},
		files:     map[int64][]*putio.File{},
	}
	if _, err := New(client, 1, root, true).Reconcile(context.Background()); err == nil {
		t.Fatal("expected unsafe transfer path to fail reconciliation")
	}
}

func TestDeleteDefaultsToDryRun(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "local.bin"), "local")
	client := &fakeClient{files: map[int64][]*putio.File{
		1: {putioFile(2, "remote.bin", 1, false, 6)},
	}}

	service := New(client, 1, root, true)
	report, err := service.Delete(context.Background(), DeleteOptions{
		IDs: []string{unmanagedObjectID(t, service, "local", "local.bin"), "putio:2"}, Putio: true, Local: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Applied || report.Summary.WouldDelete != 2 || report.Summary.SelectedBytes != 11 {
		t.Fatalf("unexpected dry-run report: %+v", report)
	}
	if len(client.deleted) != 0 {
		t.Fatalf("dry run deleted Put.io objects: %v", client.deleted)
	}
	if _, err := os.Stat(filepath.Join(root, "local.bin")); err != nil {
		t.Fatalf("dry run changed local object: %v", err)
	}
}

func TestDeleteDryRunReconcilesBatchOnce(t *testing.T) {
	client := &fakeClient{files: map[int64][]*putio.File{
		1: {
			putioFile(2, "first.bin", 1, false, 2),
			putioFile(3, "second.bin", 1, false, 3),
		},
	}}

	report, err := New(client, 1, t.TempDir(), true).Delete(context.Background(), DeleteOptions{
		IDs: []string{"putio:2", "putio:3"}, Putio: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.WouldDelete != 2 {
		t.Fatalf("unexpected dry-run report: %+v", report)
	}
	if client.transferCalls != 1 {
		t.Fatalf("GetTransfers calls = %d, want 1", client.transferCalls)
	}
}

func TestDeleteApplyRefreshesAfterDeletion(t *testing.T) {
	client := &fakeClient{
		transferStates: [][]*putio.Transfer{
			{},
			{{ID: 7, FileID: 3, Name: "second.bin", SaveParentID: 1}},
		},
		files: map[int64][]*putio.File{1: {
			putioFile(2, "first.bin", 1, false, 2),
			putioFile(3, "second.bin", 1, false, 3),
		}},
	}

	report, err := New(client, 1, t.TempDir(), true).Delete(context.Background(), DeleteOptions{
		IDs: []string{"putio:2", "putio:3"}, Putio: true, Apply: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := report.Results[0]; got.Status != "deleted" {
		t.Fatalf("first result = %+v", got)
	}
	if got := report.Results[1]; got.Status != "skipped" || got.Reason != "active_transfer" {
		t.Fatalf("second result = %+v", got)
	}
	if !reflect.DeepEqual(client.deleted, []int64{2}) {
		t.Fatalf("deleted objects = %v, want [2]", client.deleted)
	}
	if client.transferCalls != 3 {
		t.Fatalf("GetTransfers calls = %d, want 3 (inventory plus each target)", client.transferCalls)
	}
}

func TestDeleteRequiresExplicitIDsAndSourceSelection(t *testing.T) {
	service := New(&fakeClient{}, 1, t.TempDir(), true)
	tests := []DeleteOptions{
		{IDs: []string{"putio:2"}},
		{Putio: true},
		{IDs: []string{"putio:2"}, Local: true},
		{IDs: []string{"local:" + strings.Repeat("0", sha256HexLength)}, Putio: true},
	}
	for _, options := range tests {
		if _, err := service.Delete(context.Background(), options); err == nil {
			t.Fatalf("Delete(%+v) unexpectedly succeeded", options)
		}
	}
}

func TestDeleteRefusesObjectThatBecameActive(t *testing.T) {
	root := t.TempDir()
	client := &fakeClient{
		transferStates: [][]*putio.Transfer{
			{},
			{{ID: 7, FileID: 2, Name: "remote.bin", SaveParentID: 1}},
		},
		files: map[int64][]*putio.File{1: {putioFile(2, "remote.bin", 1, false, 6)}},
	}
	service := New(client, 1, root, true)
	original, err := service.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(original.Unmanaged) != 1 {
		t.Fatalf("original report did not identify candidate: %+v", original)
	}

	report, err := service.Delete(context.Background(), DeleteOptions{IDs: []string{"putio:2"}, Putio: true, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := report.Results[0]; got.Status != "skipped" || got.Reason != "active_transfer" {
		t.Fatalf("stale selection result = %+v", got)
	}
	if !report.HasFailures() || len(client.deleted) != 0 {
		t.Fatalf("active object was not safely refused: report=%+v deleted=%v", report, client.deleted)
	}
}

func TestDeleteRefusesChangedLocalSnapshot(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(t *testing.T, root string)
		change func(t *testing.T, root string)
		path   string
	}{
		{
			name: "same-path replacement",
			setup: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "leftover.bin"), "old")
			},
			change: func(t *testing.T, root string) {
				if err := os.Remove(filepath.Join(root, "leftover.bin")); err != nil {
					t.Fatal(err)
				}
				mustWrite(t, filepath.Join(root, "leftover.bin"), "new")
			},
			path: "leftover.bin",
		},
		{
			name: "directory gained child",
			setup: func(t *testing.T, root string) {
				mustMkdir(t, filepath.Join(root, "leftover"))
				mustWrite(t, filepath.Join(root, "leftover", "first.bin"), "first")
			},
			change: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "leftover", "added.bin"), "added")
			},
			path: "leftover",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			test.setup(t, root)
			service := New(&fakeClient{files: map[int64][]*putio.File{}}, 1, root, true)
			id := unmanagedObjectID(t, service, "local", test.path)
			test.change(t, root)

			report, err := service.Delete(context.Background(), DeleteOptions{
				IDs: []string{id}, Local: true, Apply: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := report.Results[0]; got.Status != "skipped" || got.Reason != "not_unmanaged" {
				t.Fatalf("changed object result = %+v", got)
			}
			if _, err := os.Lstat(filepath.Join(root, test.path)); err != nil {
				t.Fatalf("changed object was removed: %v", err)
			}
		})
	}
}

func TestDeleteKeepsPutioAndLocalSelectionsIndependent(t *testing.T) {
	t.Run("remote only", func(t *testing.T) {
		root := t.TempDir()
		mustWrite(t, filepath.Join(root, "local.bin"), "local")
		client := &fakeClient{files: map[int64][]*putio.File{1: {putioFile(2, "remote.bin", 1, false, 6)}}}

		report, err := New(client, 1, root, true).Delete(context.Background(), DeleteOptions{
			IDs: []string{"putio:2"}, Putio: true, Apply: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if report.Results[0].Status != "deleted" || !reflect.DeepEqual(client.deleted, []int64{2}) {
			t.Fatalf("unexpected remote deletion: report=%+v deleted=%v", report, client.deleted)
		}
		if _, err := os.Stat(filepath.Join(root, "local.bin")); err != nil {
			t.Fatalf("remote-only deletion changed local object: %v", err)
		}
	})

	t.Run("local only", func(t *testing.T) {
		root := t.TempDir()
		mustWrite(t, filepath.Join(root, "local.bin"), "local")
		client := &fakeClient{files: map[int64][]*putio.File{1: {putioFile(2, "remote.bin", 1, false, 6)}}}

		service := New(client, 1, root, true)
		report, err := service.Delete(context.Background(), DeleteOptions{
			IDs: []string{unmanagedObjectID(t, service, "local", "local.bin")}, Local: true, Apply: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if report.Results[0].Status != "deleted" || len(client.deleted) != 0 {
			t.Fatalf("unexpected local deletion: report=%+v deleted=%v", report, client.deleted)
		}
		if _, err := os.Stat(filepath.Join(root, "local.bin")); !os.IsNotExist(err) {
			t.Fatalf("local object still exists: %v", err)
		}
	})
}

func TestDeleteReportsPartialFailureAndContinues(t *testing.T) {
	root := t.TempDir()
	client := &fakeClient{
		files: map[int64][]*putio.File{1: {
			putioFile(2, "first.bin", 1, false, 2),
			putioFile(3, "second.bin", 1, false, 3),
		}},
		deleteErrors: map[int64]error{3: errors.New("remote refused deletion")},
	}

	report, err := New(client, 1, root, true).Delete(context.Background(), DeleteOptions{
		IDs: []string{"putio:3", "putio:2"}, Putio: true, Apply: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.Deleted != 1 || report.Summary.Failed != 1 || !report.HasFailures() {
		t.Fatalf("unexpected partial-failure summary: %+v", report)
	}
	if got := report.Results[1]; got.ID != "putio:3" || got.Status != "failed" || got.Error != "remote refused deletion" {
		t.Fatalf("failed result is not precise: %+v", got)
	}
	if !reflect.DeepEqual(client.deleted, []int64{2}) {
		t.Fatalf("successful deletion not preserved: %v", client.deleted)
	}
}

func TestDeleteRejectsTraversalAndSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "keep"), "safe")

	if err := deleteLocalObject(context.Background(), root, Object{Source: "local", Path: "../outside"}, nil); err == nil {
		t.Fatal("expected traversal path to be rejected")
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{files: map[int64][]*putio.File{}}
	service := New(client, 1, root, true)
	report, err := service.Delete(context.Background(), DeleteOptions{
		IDs: []string{unmanagedObjectID(t, service, "local", "escape")}, Local: true, Apply: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := report.Results[0]; got.Status != "failed" || !strings.Contains(got.Error, "refusing symlink") {
		t.Fatalf("symlink result = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatalf("symlink escape changed outside file: %v", err)
	}
}

func putioFile(id int64, name string, parentID int64, directory bool, size int64) *putio.File {
	contentType := "application/octet-stream"
	if directory {
		contentType = "application/x-directory"
	}
	modified := putio.Time{Time: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)}
	return &putio.File{
		ID: id, Name: name, ParentID: parentID, ContentType: contentType, Size: size, UpdatedAt: &modified,
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func objectLabels(objects []Object) []string {
	labels := make([]string, len(objects))
	for i, object := range objects {
		labels[i] = object.Source + ":" + object.Path
	}
	return labels
}

func unmanagedObjectID(t *testing.T, service *Service, source, objectPath string) string {
	t.Helper()
	report, err := service.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range report.Unmanaged {
		if object.Source == source && object.Path == objectPath {
			return object.ID
		}
	}
	t.Fatalf("unmanaged object %s:%s not found in report", source, objectPath)
	return ""
}
