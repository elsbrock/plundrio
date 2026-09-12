package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/download"
)

type activatesWhileListingClient struct {
	active bool
}

func (f *activatesWhileListingClient) GetFiles(_ context.Context, folderID int64) ([]*putio.File, error) {
	if folderID == 1 {
		return []*putio.File{putioFile(2, "candidate", 1, true, 1)}, nil
	}
	f.active = true
	return []*putio.File{putioFile(3, "file.bin", 2, false, 1)}, nil
}

func (f *activatesWhileListingClient) GetTransfers(context.Context) ([]*putio.Transfer, error) {
	if !f.active {
		return nil, nil
	}
	return []*putio.Transfer{{ID: 9, FileID: 2, Name: "candidate", SaveParentID: 1}}, nil
}

func TestReconcileRecognizesTransferIDCategoriesAndNestedPutioFolders(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "tv", "Active.Show"))
	mustWrite(t, filepath.Join(root, "tv", "Active.Show", "episode.mkv"), "active")
	mustWrite(t, filepath.Join(root, download.CategoryStateFileName), `{"10":"tv"}`)

	client := &fakeClient{
		transfers: []*putio.Transfer{{
			ID: 10, FileID: 100, Name: "Active.Show", SaveParentID: 2,
		}},
		files: map[int64][]*putio.File{
			1:   {putioFile(2, "tv", 1, true, 6)},
			2:   {putioFile(100, "Active.Show", 2, true, 6)},
			100: {putioFile(101, "episode.mkv", 100, false, 6)},
		},
	}

	report, err := New(client, 1, root, true).Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"local:tv/Active.Show", "putio:tv/Active.Show"}
	if got := objectLabels(report.Active); !reflect.DeepEqual(got, want) {
		t.Fatalf("active objects = %v, want %v", got, want)
	}
	if len(report.Unmanaged) != 0 {
		t.Fatalf("active category content reported unmanaged: %+v", report.Unmanaged)
	}
}

func TestReconcileProtectsBarePathWhenCategoryStateIsStale(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "Active.Show"))
	mustWrite(t, filepath.Join(root, "Active.Show", "episode.mkv"), "active")
	mustWrite(t, filepath.Join(root, download.CategoryStateFileName), `{"10":"tv"}`)

	client := &fakeClient{
		transfers: []*putio.Transfer{{
			ID: 10, Name: "Active.Show", SaveParentID: 1,
		}},
		files: map[int64][]*putio.File{},
	}

	report, err := New(client, 1, root, true).Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := objectLabels(report.Active), []string{"local:Active.Show"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("active objects = %v, want %v", got, want)
	}
	if len(report.Unmanaged) != 0 {
		t.Fatalf("bare active content reported unmanaged: %+v", report.Unmanaged)
	}
}

func TestReconcileSamplesOwnershipAfterEnumeratingCandidates(t *testing.T) {
	report, err := New(&activatesWhileListingClient{}, 1, t.TempDir(), true).Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := objectLabels(report.Active); !reflect.DeepEqual(got, []string{"putio:candidate"}) {
		t.Fatalf("activation during enumeration was not observed: %v", got)
	}
	if len(report.Unmanaged) != 0 {
		t.Fatalf("newly active object reported unmanaged: %+v", report.Unmanaged)
	}
}

func TestLocalReportIDsBindToTheCurrentObjectTree(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "leftover"))
	child := filepath.Join(root, "leftover", "file.bin")
	mustWrite(t, child, "same-size")
	service := New(&fakeClient{files: map[int64][]*putio.File{}}, 1, root, true)

	first := localUnmanagedObject(t, service, "leftover")
	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, child, "same-size")
	second := localUnmanagedObject(t, service, "leftover")
	if first.ID == second.ID {
		t.Fatal("same-path replacement did not change the report ID")
	}

	mustWrite(t, filepath.Join(root, "leftover", "added.bin"), "new")
	third := localUnmanagedObject(t, service, "leftover")
	if second.ID == third.ID {
		t.Fatal("adding a directory child did not change the report ID")
	}
}

func TestLocalOwnershipUsesFilesystemIdentity(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "expected.bin"), "active")
	if err := os.Link(filepath.Join(root, "expected.bin"), filepath.Join(root, "alias.bin")); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{
		transfers: []*putio.Transfer{{ID: 1, Name: "expected.bin", SaveParentID: 1}},
		files:     map[int64][]*putio.File{},
	}

	report, err := New(client, 1, root, true).Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := objectLabels(report.Active), []string{"local:alias.bin", "local:expected.bin"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("active objects = %v, want %v", got, want)
	}
}

func localUnmanagedObject(t *testing.T, service *Service, objectPath string) Object {
	t.Helper()
	report, err := service.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range report.Unmanaged {
		if object.Source == "local" && object.Path == objectPath {
			return object
		}
	}
	t.Fatalf("local unmanaged object %q not found", objectPath)
	return Object{}
}
