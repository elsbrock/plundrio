package reconcile

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/elsbrock/go-putio"
)

func TestReconcileProtectsPendingRemovalWithoutManagedTransfer(t *testing.T) {
	for _, remotePresent := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent while worker drains", true: "moved outside managed folder"}[remotePresent], func(t *testing.T) {
			root := t.TempDir()
			mustMkdir(t, filepath.Join(root, "tv", "Show"))
			// The expected file need not exist yet: protect partial worker output.
			mustWrite(t, filepath.Join(root, "tv", "Show", "partial.tmp"), "partial")
			mustMkdir(t, filepath.Join(root, ".plundrio-files"))
			mustWrite(t, filepath.Join(root, ".plundrio-files", "10.removing.json"), `"tv"`)
			mustWrite(t, filepath.Join(root, ".plundrio-files", "10.json"), `[{"name":"Show/episode.mkv","length":7}]`)
			client := &fakeClient{}
			if remotePresent {
				client.transfers = []*putio.Transfer{{ID: 10, Name: "Show", SaveParentID: 99}}
			}
			report, err := New(client, 1, root, true).Reconcile(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Unmanaged) != 0 || !reflect.DeepEqual(objectLabels(report.Active), []string{"local:tv/Show"}) {
				t.Fatalf("pending removal exposed to deletion: %+v", report)
			}
		})
	}
}

func TestReconcileProtectsFlatPendingRemovalWithStaleCategory(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "Show"))
	mustWrite(t, filepath.Join(root, "Show", "partial.tmp"), "partial")
	mustMkdir(t, filepath.Join(root, ".plundrio-files"))
	mustWrite(t, filepath.Join(root, ".plundrio-files", "10.removing.json"), `"tv"`)
	mustWrite(t, filepath.Join(root, ".plundrio-files", "10.json"), `[{"name":"Show/episode.mkv","length":7}]`)
	report, err := New(&fakeClient{}, 1, root, false).Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Unmanaged) != 0 || !reflect.DeepEqual(objectLabels(report.Active), []string{"local:Show"}) {
		t.Fatalf("flat pending removal exposed to deletion: %+v", report)
	}
}

func TestReconcilePendingRemovalFailsClosedWithoutOwnership(t *testing.T) {
	for _, manifest := range []string{"", "invalid", `[{"name":"../outside","length":1}]`} {
		t.Run(manifest, func(t *testing.T) {
			root := t.TempDir()
			mustMkdir(t, filepath.Join(root, ".plundrio-files"))
			mustWrite(t, filepath.Join(root, ".plundrio-files", "10.removing.json"), `"tv"`)
			if manifest != "" {
				mustWrite(t, filepath.Join(root, ".plundrio-files", "10.json"), manifest)
			}
			if _, err := New(&fakeClient{}, 1, root, true).Reconcile(context.Background()); err == nil {
				t.Fatal("unknown pending-removal ownership allowed reconciliation")
			}
		})
	}
}

func TestReconcileRejectsNullPendingRemovalCategory(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, ".plundrio-files"))
	mustWrite(t, filepath.Join(root, ".plundrio-files", "10.removing.json"), "null")
	mustWrite(t, filepath.Join(root, ".plundrio-files", "10.json"), `[{"name":"Show/episode.mkv","length":7}]`)
	if _, err := New(&fakeClient{}, 1, root, true).Reconcile(context.Background()); err == nil {
		t.Fatal("null pending category allowed reconciliation")
	}
}
