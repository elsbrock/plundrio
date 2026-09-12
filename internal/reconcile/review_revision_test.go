package reconcile

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/download"
)

func TestReconcileManagedDepthMatchesProcessor(t *testing.T) {
	for _, categories := range []bool{false, true} {
		t.Run(fmt.Sprint(categories), func(t *testing.T) {
			client := &fakeClient{
				files: map[int64][]*putio.File{
					1: {putioFile(2, "tv", 1, true, 0), putioFile(10, "root", 1, false, 1)},
					2: {putioFile(3, "nested", 2, true, 0), putioFile(20, "category", 2, false, 1)},
					3: {putioFile(30, "deep", 3, false, 1)},
				},
				transfers: []*putio.Transfer{
					{ID: 1, FileID: 10, SaveParentID: 1, Name: "root"},
					{ID: 2, FileID: 20, SaveParentID: 2, Name: "category"},
					{ID: 3, FileID: 30, SaveParentID: 3, Name: "deep"},
				},
			}
			report, err := New(client, 1, t.TempDir(), categories).Reconcile(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"putio:root"}
			if categories {
				want = append(want, "putio:tv/category")
			}
			if got := objectLabels(report.Active); !reflect.DeepEqual(got, want) {
				t.Fatalf("active = %v, want %v", got, want)
			}
		})
	}
}

func TestReconcileReservesInternalState(t *testing.T) {
	for _, name := range []string{".plundrio-files", ".PLUNDRIO-FILES"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			mustMkdir(t, filepath.Join(root, name))
			mustWrite(t, filepath.Join(root, name, "1.json"), "{}")
			service := New(&fakeClient{}, 1, root, false)
			report, err := service.Reconcile(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Unmanaged) != 0 {
				t.Fatalf("reserved objects exposed: %+v", report.Unmanaged)
			}
			for _, path := range []string{name, name + "/1.json", download.CategoryStateFileName} {
				if err := deleteLocalObject(context.Background(), root, Object{Path: path}, nil); err == nil || !strings.Contains(err.Error(), "reserved") {
					t.Fatalf("reserved delete %q = %v", path, err)
				}
			}
			if _, err := os.Stat(filepath.Join(root, name, "1.json")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReconcileMalformedCategoryStateFailsClosed(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, download.CategoryStateFileName), "{")
	client := &fakeClient{files: map[int64][]*putio.File{1: {putioFile(2, "file", 1, false, 1)}}}
	report, err := New(client, 1, root, false).Delete(context.Background(), DeleteOptions{IDs: []string{"putio:2"}, Putio: true, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.Failed != 1 || len(client.deleted) != 0 {
		t.Fatalf("malformed state did not block mutation: %+v", report)
	}
}

func TestDeleteBatchDoesNotRecrawlUnrelatedTrees(t *testing.T) {
	root := t.TempDir()
	unrelated := filepath.Join(root, "unrelated")
	mustMkdir(t, unrelated)
	mustWrite(t, filepath.Join(unrelated, "keep"), "keep")
	client := &fakeClient{files: map[int64][]*putio.File{
		1:    {putioFile(1000, "unrelated", 1, true, 1)},
		1000: {putioFile(1001, "keep", 1000, false, 1)},
	}}
	var ids []string
	for i := int64(2); i < 52; i++ {
		client.files[1] = append(client.files[1], putioFile(i, fmt.Sprint(i), 1, false, 1))
		ids = append(ids, fmt.Sprintf("putio:%d", i))
	}
	// Removing an unrelated local directory after the inventory would make a
	// cached whole-tree walk fail, while scoped remote checks need no local walk.
	client.onDelete = func(int64) {
		if len(client.deleted) == 1 {
			if err := os.Chmod(unrelated, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(unrelated, 0755) })
		}
	}
	report, err := New(client, 1, root, false).Delete(context.Background(), DeleteOptions{IDs: ids, Putio: true, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.Deleted != 50 {
		t.Fatalf("batch failed: %+v", report)
	}
	if client.fileCalls[1000] != 1 {
		t.Fatalf("unrelated remote subtree crawled %d times", client.fileCalls[1000])
	}
	if client.transferCalls != 51 {
		t.Fatalf("ownership checks = %d, want 51", client.transferCalls)
	}
}

func TestDeleteRechecksLocalIdentityAfterOwnershipRefresh(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "candidate"), "old")
	client := &fakeClient{}
	service := New(client, 1, root, false)
	id := unmanagedObjectID(t, service, "local", "candidate")
	client.onTransfers = func(call int) {
		// Report, batch inventory, then the selected branch's ownership refresh.
		if call == 3 {
			mustWrite(t, filepath.Join(root, "candidate"), "changed")
		}
	}
	report, err := service.Delete(context.Background(), DeleteOptions{IDs: []string{id}, Local: true, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.Failed != 1 || !strings.Contains(report.Results[0].Error, "changed") {
		t.Fatalf("changed object was not refused: %+v", report)
	}
	if data, err := os.ReadFile(filepath.Join(root, "candidate")); err != nil || string(data) != "changed" {
		t.Fatalf("replacement lost: %q %v", data, err)
	}
}

func TestDeleteDirectlyRejectsSymlinkParent(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "real"))
	mustWrite(t, filepath.Join(root, "real", "keep"), "keep")
	if err := os.Symlink("real", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := deleteLocalObject(context.Background(), root, Object{Path: "alias/keep"}, nil); err == nil || !strings.Contains(err.Error(), "symlink parent") {
		t.Fatalf("symlink parent was not refused: %v", err)
	}
}

func TestDeleteLocalBatchLeavesUnselectedTreesUnwalked(t *testing.T) {
	root := t.TempDir()
	unrelated := filepath.Join(root, "unrelated")
	mustMkdir(t, unrelated)
	mustWrite(t, filepath.Join(unrelated, "keep"), "keep")
	for i := 0; i < 10; i++ {
		mustWrite(t, filepath.Join(root, fmt.Sprintf("candidate-%d", i)), "payload")
	}
	client := &fakeClient{files: map[int64][]*putio.File{
		1:    {putioFile(1000, "unrelated", 1, true, 1)},
		1000: {putioFile(1001, "keep", 1000, false, 1)},
	}}
	service := New(client, 1, root, false)
	preview, err := service.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, object := range preview.Unmanaged {
		if strings.HasPrefix(object.Path, "candidate-") {
			ids = append(ids, object.ID)
		}
	}
	client.onTransfers = func(call int) {
		// Preview and the batch inventory must finish their content reads.
		// Revoke access during the first selected-branch refresh instead.
		if call == 3 {
			if err := os.Chmod(unrelated, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(unrelated, 0755) })
		}
	}
	report, err := service.Delete(context.Background(), DeleteOptions{IDs: ids, Local: true, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.Deleted != 10 {
		t.Fatalf("local batch failed: %+v", report)
	}
	if client.fileCalls[1000] != 2 {
		t.Fatalf("unrelated subtree calls = %d, want preview + inventory", client.fileCalls[1000])
	}
}

func TestDeleteRefusesRemoteObjectMovedBetweenTargets(t *testing.T) {
	for _, outside := range []bool{false, true} {
		t.Run(fmt.Sprint(outside), func(t *testing.T) {
			client := &fakeClient{files: map[int64][]*putio.File{
				1: {putioFile(2, "first", 1, false, 1), putioFile(3, "selected", 1, true, 1)},
				3: {putioFile(4, "payload", 3, false, 1)},
			}}
			client.onDelete = func(id int64) {
				if id == 2 {
					if outside {
						client.files[1] = client.files[1][:1]
					} else {
						client.files[1][1] = putioFile(3, "moved", 1, true, 1)
					}
				}
			}
			report, err := New(client, 1, t.TempDir(), false).Delete(context.Background(), DeleteOptions{IDs: []string{"putio:2", "putio:3"}, Putio: true, Apply: true})
			if err != nil {
				t.Fatal(err)
			}
			if report.Summary.Deleted != 1 || !reflect.DeepEqual(client.deleted, []int64{2}) {
				t.Fatalf("moved object was deleted: %+v, deleted %v", report, client.deleted)
			}
		})
	}
}

func TestDeleteScopedSnapshotProtectsNewActiveDescendant(t *testing.T) {
	client := &fakeClient{files: map[int64][]*putio.File{
		1: {putioFile(2, "first", 1, false, 1), putioFile(3, "category", 1, true, 1)},
		3: {putioFile(4, "payload", 3, false, 1)},
	}}
	client.onDelete = func(id int64) {
		if id == 2 {
			client.transfers = []*putio.Transfer{{ID: 9, FileID: 4, SaveParentID: 3, Name: "payload"}}
		}
	}
	report, err := New(client, 1, t.TempDir(), true).Delete(context.Background(), DeleteOptions{IDs: []string{"putio:2", "putio:3"}, Putio: true, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.Deleted != 1 || !reflect.DeepEqual(client.deleted, []int64{2}) {
		t.Fatalf("active descendant was deleted with ancestor: %+v", report)
	}
}
