package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/api"
	"github.com/elsbrock/plundrio/internal/config"
	"github.com/elsbrock/plundrio/internal/download"
)

type reviewRPCClient struct{ *torrentAddClient }

func (c *reviewRPCClient) GetAllTransferFiles(_ context.Context, id int64) ([]*putio.File, error) {
	return nil, &api.TransferSourceNotFoundError{FileID: id, Err: &putio.ErrorResponse{Type: "NotFound"}}
}
func (c *reviewRPCClient) GetFiles(context.Context, int64) ([]*putio.File, error) { return nil, nil }
func (c *reviewRPCClient) RetryTransfer(context.Context, int64) (*putio.Transfer, error) {
	return nil, errors.New("unexpected retry")
}
func (c *reviewRPCClient) GetDownloadURL(context.Context, int64) (string, error) {
	return "", errors.New("unexpected download")
}

// Exercise the real manager's durable-state validation without starting workers.
type reviewRPCService struct {
	*download.Manager
	transfers []*putio.Transfer
}

func (s *reviewRPCService) GetTransfers() []*putio.Transfer { return s.transfers }

type classificationService struct {
	*torrentAddDownloadService
	local *download.TransferContext
}

func (s *classificationService) GetTransferContext(int64) (*download.TransferContext, bool) {
	return s.local, s.local != nil
}

func (s *classificationService) PrepareRemoval(id int64, requireClassified bool) (string, error) {
	if requireClassified && (s.local == nil || s.local.GetState() == download.TransferLifecycleInitial) {
		return "", errors.New("classification pending")
	}
	return s.torrentAddDownloadService.PrepareRemoval(id, requireClassified)
}

func TestOrdinaryRemovalWaitsForReadyTransferClassification(t *testing.T) {
	for _, local := range []*download.TransferContext{nil, download.NewTransferContext(101, 0, download.TransferLifecycleInitial)} {
		client := &torrentAddClient{transfers: []*putio.Transfer{{ID: 101, FileID: 500, Name: "Book", Status: "COMPLETED"}}}
		service := &classificationService{torrentAddDownloadService: &torrentAddDownloadService{}, local: local}
		srv := &Server{cfg: &config.Config{TargetDir: t.TempDir()}, client: client, dlService: service}
		if _, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[101],"delete-local-data":true}`)); err == nil {
			t.Fatal("unclassified ready record removal accepted")
		}
		if len(client.deleted) != 0 || len(client.deletedFiles) != 0 || service.RemovalPending(101) {
			t.Fatal("classification guard mutated state")
		}
	}
}

func newReviewRPCServer(t *testing.T, sourceID int64) (*Server, *reviewRPCClient, *download.Manager) {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, ".plundrio-files")
	if err := os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	marker := fmt.Sprintf(`{"id":101,"file_id":%d,"name":"Book","save_parent_id":42,"category":"books"}`, sourceID)
	if err := os.WriteFile(filepath.Join(state, "101.review.json"), []byte(marker), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "books", "Book"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "books", "Book", "keep"), []byte("retained payload"), 0600); err != nil {
		t.Fatal(err)
	}
	transfer := &putio.Transfer{ID: 101, FileID: sourceID, Name: "Book", SaveParentID: 42, Status: "COMPLETED", PercentDone: 100, DownloadSpeed: 123, UploadSpeed: 456}
	client := &reviewRPCClient{&torrentAddClient{transfers: []*putio.Transfer{transfer}}}
	cfg := &config.Config{TargetDir: root, FolderID: 42, UseCategoriesTarget: true}
	manager := download.New(cfg, client)
	return &Server{cfg: cfg, client: client, dlService: &reviewRPCService{manager, client.transfers}}, client, manager
}

func TestReviewRPCWarningNeverClaimsCompletion(t *testing.T) {
	for _, sourceID := range []int64{0, 500} {
		for _, size := range []int{0, 1000} {
			t.Run(fmt.Sprintf("%d/%d", sourceID, size), func(t *testing.T) {
				srv, client, _ := newReviewRPCServer(t, sourceID)
				client.transfers[0].Size = size
				response, err := srv.handleTorrentGet(context.Background(), json.RawMessage(`{"fields":["files"]}`))
				if err != nil {
					t.Fatal(err)
				}
				info := response.(map[string]interface{})["torrents"].([]map[string]interface{})[0]
				if info["status"] != trStatusStopped || info["percentDone"] != 0.5 || info["leftUntilDone"].(int64) <= 0 || info["error"] != false || info["errorString"] != download.NeedsReviewMessage || info["rateDownload"] != 0 || info["rateUpload"] != 0 || info["eta"] != -1 {
					t.Fatalf("unsafe review RPC: %+v", info)
				}
				if len(info["files"].([]transmissionFile)) != 0 {
					t.Fatal("invented ownership")
				}
				if info["seedRatioMode"] != transmissionLimitModeUnlimited || info["seedIdleMode"] != transmissionLimitModeUnlimited || info["secondsSeeding"] != int64(0) {
					t.Fatalf("review must not enable automatic removal: %+v", info)
				}
			})
		}
	}
}

func TestReviewRetirementRejectsImplicitOrAmbiguousRequests(t *testing.T) {
	for _, args := range []string{
		`{"ids":[101],"delete-local-data":false}`,
		`{"ids":[101],"delete-local-data":true}`,
		`{"ids":[101],"plundrio-retire-reviewed":true}`,
		`{"ids":[101],"plundrio-copy-verified":true}`,
		`{"ids":[101],"plundrio-retire-reviewed":true,"plundrio-copy-verified":true,"delete-local-data":true}`,
		`{"ids":[],"plundrio-retire-reviewed":true,"plundrio-copy-verified":true}`,
		`{"ids":[101,102],"plundrio-retire-reviewed":true,"plundrio-copy-verified":true}`,
		`{"ids":["hash"],"plundrio-retire-reviewed":true,"plundrio-copy-verified":true}`,
		`{"ids":[0],"plundrio-retire-reviewed":true,"plundrio-copy-verified":true}`,
	} {
		t.Run(args, func(t *testing.T) {
			srv, client, manager := newReviewRPCServer(t, 500)
			if _, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(args)); err == nil {
				t.Fatal("unsafe request accepted")
			}
			if len(client.deleted) != 0 || len(client.deletedFiles) != 0 || manager.RemovalPending(101) {
				t.Fatal("invalid request mutated state")
			}
		})
	}
}

func TestReviewRetirementPreservesFilesAndCanRetryAfterRestart(t *testing.T) {
	for _, sourceID := range []int64{0, 500} {
		t.Run(fmt.Sprint(sourceID), func(t *testing.T) {
			srv, client, manager := newReviewRPCServer(t, sourceID)
			args := json.RawMessage(`{"ids":[101],"delete-local-data":false,"plundrio-retire-reviewed":true,"plundrio-copy-verified":true}`)
			client.deleteTransferErr = errors.New("temporary failure")
			if _, err := srv.handleTorrentRemove(context.Background(), args); err == nil {
				t.Fatal("expected failed retirement")
			}
			if len(client.deleted) != 3 || !manager.RemovalPending(101) || !manager.NeedsReview(101) {
				t.Fatal("failure lost durable holds/bounded retries")
			}
			response, err := srv.handleTorrentGet(context.Background(), json.RawMessage(`{"fields":["files"]}`))
			if err != nil {
				t.Fatal(err)
			}
			info := response.(map[string]interface{})["torrents"].([]map[string]interface{})[0]
			if info["error"] != false || info["percentDone"] != 0.5 || info["plundrioState"] != "needs-review" {
				t.Fatalf("failed retirement must remain a review warning, got %+v", info)
			}
			restarted := download.New(srv.cfg, client)
			srv.dlService = &reviewRPCService{restarted, client.transfers}
			if _, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[101],"delete-local-data":true}`)); err == nil {
				t.Fatal("generic retry bypassed review")
			}
			client.deleteTransferErr = nil
			if _, err := srv.handleTorrentRemove(context.Background(), args); err != nil {
				t.Fatal(err)
			}
			if restarted.NeedsReview(101) || restarted.RemovalPending(101) {
				t.Fatal("successful retirement left hold")
			}
			if len(client.deletedFiles) != 0 {
				t.Fatalf("retirement deleted source: %v", client.deletedFiles)
			}
			data, err := os.ReadFile(filepath.Join(srv.cfg.TargetDir, "books", "Book", "keep"))
			if err != nil || string(data) != "retained payload" {
				t.Fatal("retirement touched local payload")
			}
		})
	}
}

func TestCalculateProgressNeedsReview(t *testing.T) {
	for _, size := range []int{0, 1000} {
		progress := calculateProgress(progressInput{PutioStatus: "COMPLETED", PutioPercentDone: 100, PutioSize: size, TransferCtx: download.NewTransferContext(101, 0, download.TransferLifecycleNeedsReview)})
		if progress.Status != trStatusStopped || progress.PercentDone != 0.5 || progress.LeftUntilDone <= 0 {
			t.Fatalf("unsafe progress: %+v", progress)
		}
	}
}
