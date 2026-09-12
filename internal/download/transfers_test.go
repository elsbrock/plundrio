package download

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/api"
	"github.com/elsbrock/plundrio/internal/config"
)

func TestIsPutioNotFoundUnwrapsClientErrors(t *testing.T) {
	notFound := &putio.ErrorResponse{Type: "NotFound"}

	if !isPutioNotFound(notFound) {
		t.Fatal("expected direct Put.io NotFound error to be recognized")
	}
	if !isPutioNotFound(fmt.Errorf("get transfer files: %w", notFound)) {
		t.Fatal("expected wrapped Put.io NotFound error to be recognized")
	}
	if !isPutioNotFound(fmt.Errorf("process transfer: %w", fmt.Errorf("get transfer files: %w", notFound))) {
		t.Fatal("expected doubly wrapped Put.io NotFound error to be recognized")
	}
	if isPutioNotFound(&putio.ErrorResponse{Type: "Other"}) {
		t.Fatal("did not expect a different Put.io error type to match")
	}
	if isPutioNotFound(errors.New("not found")) {
		t.Fatal("did not expect an unrelated error to match")
	}
}

func TestHandleTransferErrorHoldsWrappedRootNotFoundWithoutManifest(t *testing.T) {
	manager := newTestProcessor(t, 42, false, &fakePutioClient{}).manager
	transfer := &putio.Transfer{ID: 1, Name: "Example", FileID: 2}
	err := fmt.Errorf("get transfer files: %w", &api.TransferSourceNotFoundError{FileID: 2, Err: &putio.ErrorResponse{Type: "NotFound"}})

	manager.processor.handleTransferError(transfer, err)

	transferContext, ok := manager.coordinator.GetTransferContext(transfer.ID)
	if !ok {
		t.Fatal("expected wrapped root NotFound to initialize review tracking")
	}
	if got := transferContext.GetState(); got != TransferLifecycleNeedsReview {
		t.Fatalf("transfer state = %s, want NeedsReview", got)
	}
}

func TestHandleTransferErrorLeavesUnrelatedErrorsUntracked(t *testing.T) {
	manager := newTestManager()
	transfer := &putio.Transfer{ID: 1, Name: "Example", FileID: 2}

	manager.processor.handleTransferError(transfer, errors.New("temporary API failure"))

	if _, ok := manager.coordinator.GetTransferContext(transfer.ID); ok {
		t.Fatal("did not expect unrelated error to initialize transfer cleanup")
	}
}

const dirContentType = "application/x-directory"

// fakePutioClient is a minimal PutioClient for exercising the transfer monitor.
// Only the methods used by the tests are wired up; the rest return zero values.
type fakePutioClient struct {
	files            []*putio.File
	filesErr         error
	allTransferFiles []*putio.File

	getFilesCalls            int
	getAllTransferFilesCalls int
}

func (f *fakePutioClient) GetTransfers(ctx context.Context) ([]*putio.Transfer, error) {
	return nil, nil
}

func (f *fakePutioClient) GetAllTransferFiles(ctx context.Context, fileID int64) ([]*putio.File, error) {
	f.getAllTransferFilesCalls++
	return f.allTransferFiles, nil
}

func TestProcessTransferPersistsExactFileManifest(t *testing.T) {
	client := &fakePutioClient{allTransferFiles: []*putio.File{
		{ID: 11, Name: "one.m4b", Size: 3},
		{ID: 12, Name: "two.epub", Size: 6},
	}}
	p := newTestProcessor(t, 100, false, client)
	transfer := &putio.Transfer{ID: 101, Name: "Book", FileID: 500}

	p.processTransfer(transfer)

	got, ok := p.manager.transferFiles.Get(101)
	want := []TransferFile{
		{Name: "Book/one.m4b", Length: 3},
		{Name: "Book/two.epub", Length: 6},
	}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("manifest = %+v, exists=%v, want %+v", got, ok, want)
	}
}

func TestBuildTransferFileManifestRejectsUnsafeOrCollidingPaths(t *testing.T) {
	transfer := &putio.Transfer{ID: 101, Name: "Book"}
	if _, err := buildTransferFileManifest(transfer, []*putio.File{{Name: "../escape.m4b", Size: 1}}); err == nil {
		t.Fatal("expected escaping file name to fail")
	}
	if _, err := buildTransferFileManifest(transfer, []*putio.File{
		{Name: "same.m4b", Size: 1},
		{Name: "same.m4b", Size: 1},
	}); err == nil {
		t.Fatal("expected colliding local file names to fail")
	}
	transfer.Name = transferFilesStateDirName
	if _, err := buildTransferFileManifest(transfer, []*putio.File{{Name: "101.json", Size: 1}}); err == nil {
		t.Fatal("expected reserved manifest directory name to fail")
	}
}

func TestProcessTransferManifestFailureIsMarkedFailed(t *testing.T) {
	client := &fakePutioClient{allTransferFiles: []*putio.File{
		{ID: 11, Name: "same.m4b", Size: 3},
		{ID: 12, Name: "same.m4b", Size: 6},
	}}
	p := newTestProcessor(t, 100, false, client)
	transfer := &putio.Transfer{ID: 101, Name: "Book", FileID: 500}

	p.processTransfer(transfer)

	ctx, ok := p.manager.coordinator.GetTransferContext(transfer.ID)
	if !ok {
		t.Fatal("expected manifest failure to initialize transfer tracking")
	}
	if state := ctx.GetState(); state != TransferLifecycleFailed {
		t.Fatalf("transfer state = %s, want Failed", state)
	}
	if err := ctx.GetError(); err == nil {
		t.Fatal("expected manifest failure reason to be recorded")
	}
}

func TestProcessTransferManifestPersistenceFailureIsMarkedFailed(t *testing.T) {
	client := &fakePutioClient{allTransferFiles: []*putio.File{
		{ID: 11, Name: "book.m4b", Size: 3},
	}}
	p := newTestProcessor(t, 100, false, client)
	transfer := &putio.Transfer{ID: 101, Name: "Book", FileID: 500}
	if err := os.WriteFile(p.manager.transferFiles.stateDir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}

	p.processTransfer(transfer)

	ctx, ok := p.manager.coordinator.GetTransferContext(transfer.ID)
	if !ok || ctx.GetState() != TransferLifecycleFailed {
		t.Fatalf("manifest persistence failure was not tracked as Failed: context=%v exists=%v", ctx, ok)
	}
	if err := ctx.GetError(); err == nil {
		t.Fatal("expected manifest persistence failure reason to be recorded")
	}
}

func TestProcessTransferCompletesWhenAllFilesAlreadyExist(t *testing.T) {
	client := &fakePutioClient{allTransferFiles: []*putio.File{
		{ID: 11, Name: "book.m4b", Size: 4},
	}}
	p := newTestProcessor(t, 100, false, client)
	transfer := &putio.Transfer{ID: 101, Name: "Book", FileID: 500}
	path := filepath.Join(p.targetDir, transfer.Name, "book.m4b")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("book"), 0644); err != nil {
		t.Fatal(err)
	}

	p.processTransfer(transfer)

	ctx, ok := p.manager.coordinator.GetTransferContext(transfer.ID)
	if !ok || ctx.GetState() != TransferLifecycleProcessed {
		t.Fatalf("existing files were not completed and processed: context=%v exists=%v", ctx, ok)
	}
}

func (f *fakePutioClient) GetFiles(ctx context.Context, folderID int64) ([]*putio.File, error) {
	f.getFilesCalls++
	return f.files, f.filesErr
}

func TestProcessTransferHoldsLegacyCleanedTransferWithoutManifest(t *testing.T) {
	client := &fakePutioClient{}
	p := newTestProcessor(t, 100, false, client)
	transfer := &putio.Transfer{ID: 101, Name: "already cleaned", FileID: 0}

	p.processTransfer(transfer)

	if client.getAllTransferFilesCalls != 0 {
		t.Fatalf("GetAllTransferFiles called %d times, want 0 for Put.io root", client.getAllTransferFilesCalls)
	}
	ctx, ok := p.manager.coordinator.GetTransferContext(101)
	if !ok || ctx.GetState() != TransferLifecycleNeedsReview {
		t.Fatalf("legacy transfer was not held for review: context=%v exists=%v", ctx, ok)
	}
}

func TestProcessTransferRejectsInvalidExistingManifestAsLegacy(t *testing.T) {
	for _, contents := range []string{"corrupt", "[]", "null", "directory"} {
		t.Run(contents, func(t *testing.T) {
			client := &fakePutioClient{}
			p := newTestProcessor(t, 100, false, client)
			path := p.manager.transferFiles.path(101)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if contents == "directory" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			p.processTransfer(&putio.Transfer{ID: 101, Name: "Book", FileID: 0})
			ctx, ok := p.manager.GetTransferContext(101)
			if !ok || ctx.GetState() != TransferLifecycleFailed || ctx.GetError() == nil {
				t.Fatalf("invalid existing manifest restored as legacy completion: context=%v exists=%v", ctx, ok)
			}
			if client.getAllTransferFilesCalls != 0 {
				t.Fatal("invalid manifest caused Put.io root lookup")
			}
		})
	}
}

func TestProcessTransferRestoresCleanedTransferFromManifest(t *testing.T) {
	client := &fakePutioClient{}
	p := newTestProcessor(t, 100, false, client)
	transfer := &putio.Transfer{ID: 101, Name: "already cleaned", FileID: 0}
	path := filepath.Join(p.targetDir, transfer.Name, "book.m4b")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("book"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := p.manager.transferFiles.Set(101, []TransferFile{{Name: "already cleaned/book.m4b", Length: 4}}); err != nil {
		t.Fatal(err)
	}

	p.processTransfer(transfer)

	if client.getAllTransferFilesCalls != 0 {
		t.Fatalf("GetAllTransferFiles called %d times, want 0 for Put.io root", client.getAllTransferFilesCalls)
	}
	ctx, ok := p.manager.coordinator.GetTransferContext(101)
	if !ok || ctx.GetState() != TransferLifecycleProcessed {
		t.Fatalf("transfer was not restored as processed: context=%v exists=%v", ctx, ok)
	}
}

func (f *fakePutioClient) RetryTransfer(ctx context.Context, transferID int64) (*putio.Transfer, error) {
	return nil, nil
}

func (f *fakePutioClient) DeleteTransfer(ctx context.Context, transferID int64) error { return nil }

func (f *fakePutioClient) DeleteFile(ctx context.Context, fileID int64) error { return nil }

func (f *fakePutioClient) GetDownloadURL(ctx context.Context, fileID int64) (string, error) {
	return "", nil
}

func newTestProcessor(t *testing.T, folderID int64, usePutio bool, client PutioClient) *TransferProcessor {
	t.Helper()
	cfg := &config.Config{
		TargetDir:          t.TempDir(),
		FolderID:           folderID,
		UseCategoriesPutio: usePutio,
	}
	m := New(cfg, client)
	return m.processor
}

func dirFile(id int64, name string) *putio.File {
	return &putio.File{ID: id, Name: name, ContentType: dirContentType}
}

func plainFile(id int64, name string) *putio.File {
	return &putio.File{ID: id, Name: name, ContentType: "video/mp4"}
}

func TestManagedFolders_PutioCategoriesDisabled(t *testing.T) {
	client := &fakePutioClient{
		files: []*putio.File{dirFile(11, "tv"), dirFile(12, "movies")},
	}
	p := newTestProcessor(t, 100, false, client)

	managed := p.managedFolders()

	if len(managed) != 1 {
		t.Fatalf("managed folders = %v, want only the configured folder", managed)
	}
	if _, ok := managed[100]; !ok {
		t.Errorf("managed folders missing configured folder 100: %v", managed)
	}
	if client.getFilesCalls != 0 {
		t.Errorf("GetFiles called %d times, want 0 when put.io categories disabled", client.getFilesCalls)
	}
}

func TestManagedFolders_PutioCategoriesEnabled(t *testing.T) {
	client := &fakePutioClient{
		files: []*putio.File{
			dirFile(11, "tv"),
			dirFile(12, "movies"),
			plainFile(13, "loose-file.mkv"), // non-directory must be ignored
		},
	}
	p := newTestProcessor(t, 100, true, client)

	managed := p.managedFolders()

	for _, want := range []int64{100, 11, 12} {
		if _, ok := managed[want]; !ok {
			t.Errorf("managed folders missing %d: %v", want, managed)
		}
	}
	if _, ok := managed[13]; ok {
		t.Errorf("managed folders should not include non-directory 13: %v", managed)
	}
	if client.getFilesCalls != 1 {
		t.Errorf("GetFiles called %d times, want 1", client.getFilesCalls)
	}
}

func TestManagedFolders_ListErrorFallsBackToConfiguredFolder(t *testing.T) {
	client := &fakePutioClient{filesErr: errors.New("boom")}
	p := newTestProcessor(t, 100, true, client)

	managed := p.managedFolders()

	if len(managed) != 1 {
		t.Fatalf("managed folders = %v, want only the configured folder on list error", managed)
	}
	if _, ok := managed[100]; !ok {
		t.Errorf("managed folders missing configured folder 100 on list error: %v", managed)
	}
}

func TestLocalCategory_GatedByTargetFlag(t *testing.T) {
	tests := []struct {
		name      string
		useTarget bool
		want      string
	}{
		{name: "enabled returns stored category", useTarget: true, want: "tv"},
		{name: "disabled returns empty", useTarget: false, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				TargetDir:           t.TempDir(),
				FolderID:            100,
				UseCategoriesTarget: tt.useTarget,
			}
			m := New(cfg, &fakePutioClient{})
			m.SetCategory(42, "tv")

			if got := m.localCategory(42); got != tt.want {
				t.Errorf("localCategory(42) = %q, want %q", got, tt.want)
			}
		})
	}
}
