package download

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/api"
)

// Neither missing-source shape proves that the old instance finished locally.
func TestLegacyMissingProofRequiresReview(t *testing.T) {
	for _, sourceID := range []int64{0, 500} {
		t.Run(fmt.Sprint(sourceID), func(t *testing.T) {
			p := newManagerForTest(t, &fakeClient{files: func(id int64) ([]*putio.File, error) {
				if id == 0 {
					t.Fatal("queried Put.io root")
				}
				return nil, fmt.Errorf("wrapped: %w", &api.TransferSourceNotFoundError{FileID: id, Err: &putio.ErrorResponse{Type: "NotFound"}})
			}}).processor
			transfer := &putio.Transfer{ID: 101, FileID: sourceID, Name: "Book", Status: "COMPLETED"}
			p.processTransfer(transfer)
			ctx, ok := p.manager.GetTransferContext(101)
			if !ok || ctx.GetState().String() != "NeedsReview" {
				t.Fatalf("missing proof must be NeedsReview, got %+v", ctx)
			}
			if ctx.FileID != sourceID || ctx.TotalFiles != 0 || ctx.GetError() != nil {
				t.Fatalf("review must preserve source identity without fabricated work/failure: %+v", ctx)
			}
			for range 5 {
				if p.shouldProcess(transfer) {
					t.Fatal("review record must not automatically reprocess")
				}
			}
			if files, ok := p.manager.GetTransferFiles(101); ok || len(files) != 0 {
				t.Fatal("review must not invent a manifest")
			}
		})
	}
}

type reviewMonitorClient struct {
	*fakeClient
	retries, deletions int
}

func (c *reviewMonitorClient) RetryTransfer(context.Context, int64) (*putio.Transfer, error) {
	c.retries++
	return &putio.Transfer{}, nil
}
func (c *reviewMonitorClient) DeleteTransfer(context.Context, int64) error { c.deletions++; return nil }

func TestReviewMonitorHoldsRemoteErrorsAcrossRestart(t *testing.T) {
	transfer := &putio.Transfer{ID: 101, FileID: 0, Name: "Book", Status: "COMPLETED", SaveParentID: 42}
	client := &reviewMonitorClient{fakeClient: &fakeClient{transfers: func() ([]*putio.Transfer, error) { return []*putio.Transfer{transfer}, nil }, files: func(int64) ([]*putio.File, error) { t.Fatal("held record queried recovered source"); return nil, nil }}}
	m := newManagerForTest(t, client)
	m.processor.processTransfer(transfer)
	transfer.Status, transfer.FileID = "ERROR", 500
	for _, instance := range []*Manager{m, New(m.cfg, client)} {
		for range 5 {
			instance.processor.checkTransfers()
			instance.processorWg.Wait()
		}
		ctx, ok := instance.GetTransferContext(101)
		if !ok || ctx.GetState() != TransferLifecycleNeedsReview {
			t.Fatal("monitor lost review hold")
		}
	}
	if client.retries != 0 || client.deletions != 0 {
		t.Fatal("monitor retried/deleted held record")
	}
}

func TestMissingChildAndExistingManifestAreNotLegacyAbsence(t *testing.T) {
	for _, scenario := range []string{"child404", "corrupt", "empty", "dangling"} {
		t.Run(scenario, func(t *testing.T) {
			m := newManagerForTest(t, &fakeClient{files: func(id int64) ([]*putio.File, error) {
				err := &putio.ErrorResponse{Type: "NotFound"}
				if scenario == "child404" {
					return nil, fmt.Errorf("child listing: %w", err)
				}
				return nil, &api.TransferSourceNotFoundError{FileID: id, Err: err}
			}})
			if scenario != "child404" {
				if err := os.MkdirAll(m.transferFiles.stateDir, 0700); err != nil {
					t.Fatal(err)
				}
				var err error
				if scenario == "dangling" {
					err = os.Symlink(filepath.Join(m.cfg.TargetDir, "missing"), m.transferFiles.path(101))
				} else {
					data := "{"
					if scenario == "empty" {
						data = "[]"
					}
					err = os.WriteFile(m.transferFiles.path(101), []byte(data), 0600)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			m.processor.processTransfer(&putio.Transfer{ID: 101, Name: "Book", FileID: 500})
			ctx, ok := m.GetTransferContext(101)
			if !ok || ctx.GetState() != TransferLifecycleFailed || ctx.GetError() == nil || m.NeedsReview(101) {
				t.Fatalf("ordinary failure became review/completion: %+v", ctx)
			}
		})
	}
}
