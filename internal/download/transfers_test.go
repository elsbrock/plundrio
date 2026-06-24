package download

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/elsbrock/go-putio"
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

func TestHandleTransferErrorCleansUpWrappedNotFound(t *testing.T) {
	manager := newTestManager()
	transfer := &putio.Transfer{ID: 1, Name: "Example", FileID: 2}
	err := fmt.Errorf("get transfer files: %w", &putio.ErrorResponse{Type: "NotFound"})

	manager.processor.handleTransferError(transfer, err)

	transferContext, ok := manager.coordinator.GetTransferContext(transfer.ID)
	if !ok {
		t.Fatal("expected wrapped NotFound error to initialize transfer cleanup")
	}
	if got := transferContext.GetState(); got != TransferLifecycleProcessed {
		t.Fatalf("transfer state = %s, want Processed", got)
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
	files    []*putio.File
	filesErr error

	getFilesCalls int
}

func (f *fakePutioClient) GetTransfers(ctx context.Context) ([]*putio.Transfer, error) {
	return nil, nil
}

func (f *fakePutioClient) GetAllTransferFiles(ctx context.Context, fileID int64) ([]*putio.File, error) {
	return nil, nil
}

func (f *fakePutioClient) GetFiles(ctx context.Context, folderID int64) ([]*putio.File, error) {
	f.getFilesCalls++
	return f.files, f.filesErr
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
