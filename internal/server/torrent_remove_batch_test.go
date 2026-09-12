package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/config"
	"github.com/elsbrock/plundrio/internal/download"
)

type batchRemovalClient struct {
	*torrentAddClient
	failures map[int64]error
}

func (c *batchRemovalClient) DeleteTransfer(_ context.Context, id int64) error {
	c.deleted = append(c.deleted, id)
	return c.failures[id]
}

func TestTorrentRemoveContinuesBatchAfterFailure(t *testing.T) {
	remoteErr := errors.New("remote deletion failed")
	for _, tt := range []struct {
		name           string
		unclassified   bool
		remoteFailures map[int64]error
		wantAttempts   []int64
		wantFiles      []int64
		wantErrors     []string
	}{
		{
			name:         "classification failure",
			unclassified: true,
			wantAttempts: []int64{202, 303},
			wantFiles:    []int64{502, 503},
			wantErrors:   []string{"preserve removal state for transfer 101"},
		},
		{
			name:           "exhausted remote deletion",
			remoteFailures: map[int64]error{101: remoteErr},
			wantAttempts:   []int64{101, 101, 101, 202, 303},
			wantFiles:      []int64{501, 502, 503},
			wantErrors:     []string{"remove transfer 101 after at most 3 attempts"},
		},
		{
			name:           "multiple failures",
			unclassified:   true,
			remoteFailures: map[int64]error{202: remoteErr},
			wantAttempts:   []int64{202, 202, 202, 303},
			wantFiles:      []int64{502, 503},
			wantErrors:     []string{"preserve removal state for transfer 101", "remove transfer 202 after at most 3 attempts"},
		},
		{
			name:         "successful batch",
			wantAttempts: []int64{101, 202, 303},
			wantFiles:    []int64{501, 502, 503},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			cfg := &config.Config{TargetDir: root, UseCategoriesTarget: true}
			manager := download.New(cfg, nil)
			transfers := []*putio.Transfer{
				{ID: 101, Name: "First", FileID: 501, Status: "DOWNLOADING"},
				{ID: 202, Name: "Second", FileID: 502, Status: "DOWNLOADING"},
				{ID: 303, Name: "Third", FileID: 503, Status: "DOWNLOADING"},
			}
			if tt.unclassified {
				transfers[0].Status = "COMPLETED"
			}
			stateDir := filepath.Join(root, ".plundrio-files")
			if err := os.MkdirAll(stateDir, 0700); err != nil {
				t.Fatal(err)
			}
			for _, transfer := range transfers {
				manager.SetCategory(transfer.ID, "books")
				payload := filepath.Join(root, "books", transfer.Name)
				if err := os.WriteFile(filepath.Join(stateDir, fmt.Sprintf("%d.json", transfer.ID)), []byte(fmt.Sprintf(`[{"name":%q,"length":7}]`, transfer.Name)), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(payload), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(payload, []byte("payload"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			client := &batchRemovalClient{torrentAddClient: &torrentAddClient{transfers: transfers}, failures: tt.remoteFailures}
			srv := &Server{cfg: cfg, client: client, dlService: manager}
			// An absent selector must also leave subsequent IDs eligible for removal.
			_, err := srv.handleTorrentRemove(context.Background(), json.RawMessage(`{"ids":[999,101,202,303],"delete-local-data":true}`))
			if len(tt.wantErrors) == 0 && err != nil {
				t.Errorf("successful batch returned %v", err)
			}
			for _, want := range tt.wantErrors {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want %q", err, want)
				}
			}
			if len(tt.remoteFailures) > 0 && !errors.Is(err, remoteErr) {
				t.Errorf("error = %v, want wrapped remote failure", err)
			}
			if !reflect.DeepEqual(client.deleted, tt.wantAttempts) {
				t.Errorf("remote attempts = %v, want %v", client.deleted, tt.wantAttempts)
			}
			if !reflect.DeepEqual(client.deletedFiles, tt.wantFiles) {
				t.Errorf("source deletions = %v, want %v", client.deletedFiles, tt.wantFiles)
			}
			for _, transfer := range transfers {
				failed := (tt.unclassified && transfer.ID == 101) || tt.remoteFailures[transfer.ID] != nil
				if got := manager.RemovalPending(transfer.ID); got != (tt.remoteFailures[transfer.ID] != nil) {
					t.Errorf("transfer %d removal pending = %v", transfer.ID, got)
				}
				if _, present := manager.GetTransferFiles(transfer.ID); present != failed {
					t.Errorf("transfer %d manifest present = %v, want %v", transfer.ID, present, failed)
				}
				wantCategory := ""
				if failed {
					wantCategory = "books"
				}
				if got := manager.GetCategory(transfer.ID); got != wantCategory {
					t.Errorf("transfer %d category = %q, want %q", transfer.ID, got, wantCategory)
				}
				data, statErr := os.ReadFile(filepath.Join(root, "books", transfer.Name))
				if failed && (statErr != nil || string(data) != "payload") {
					t.Errorf("transfer %d failed removal lost local payload: %v", transfer.ID, statErr)
				} else if !failed && !os.IsNotExist(statErr) {
					t.Errorf("transfer %d successful removal retained local payload: %v", transfer.ID, statErr)
				}
			}
		})
	}
}
