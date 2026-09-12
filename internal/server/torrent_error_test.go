package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/config"
	"github.com/elsbrock/plundrio/internal/download"
)

func TestHandleTorrentGetUsesTransmissionErrorFields(t *testing.T) {
	tests := []struct {
		name        string
		errorString string
		wantError   int
	}{
		{name: "healthy", wantError: 0},
		{
			name:        "sustained zero progress",
			errorString: "Put.io transfer stalled: no byte progress for 6h0m0s; inspect the transfer in Put.io",
			wantError:   3,
		},
		{
			name:        "Put.io error",
			errorString: "source unavailable",
			wantError:   3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transfer := &putio.Transfer{
				ID:           42,
				Hash:         "hash",
				Name:         "example",
				Status:       "DOWNLOADING",
				ErrorMessage: tt.errorString,
			}
			service := &torrentAddDownloadService{
				transfers: []*putio.Transfer{transfer},
			}
			srv := &Server{
				cfg:       &config.Config{TargetDir: t.TempDir()},
				dlService: service,
			}

			result, err := srv.handleTorrentGet(context.Background(), json.RawMessage(`{"fields":["error","errorString"]}`))
			if err != nil {
				t.Fatalf("handleTorrentGet() error = %v", err)
			}
			torrents := result.(map[string]interface{})["torrents"].([]map[string]interface{})
			if len(torrents) != 1 {
				t.Fatalf("torrent count = %d, want 1", len(torrents))
			}
			if got := torrents[0]["error"]; got != tt.wantError {
				t.Errorf("error = %#v, want %d", got, tt.wantError)
			}
			if got := torrents[0]["errorString"]; got != tt.errorString {
				t.Errorf("errorString = %#v, want %q", got, tt.errorString)
			}
		})
	}
}

func TestTorrentGetReportsLocalOwnershipFailure(t *testing.T) {
	coordinator := download.NewTransferCoordinator()
	ctx := coordinator.InitiateTransfer(42, "Book", 0, 0)
	want := "parse transfer file state: invalid manifest"
	if err := coordinator.FailTransfer(42, errors.New(want)); err != nil {
		t.Fatal(err)
	}
	service := &torrentAddDownloadService{
		transfers: []*putio.Transfer{{ID: 42, Name: "Book", Status: "COMPLETED", PercentDone: 100, ErrorMessage: "older remote error"}},
		contexts:  map[int64]*download.TransferContext{42: ctx},
	}
	server := &Server{cfg: &config.Config{TargetDir: t.TempDir()}, dlService: service}
	result, err := server.handleTorrentGet(context.Background(), json.RawMessage(`{"fields":["error","errorString","seedIdleMode"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Torrents []struct {
			Error        int
			ErrorString  string
			SeedIdleMode int
		}
	}
	decodeResponse(t, result, &decoded)
	if len(decoded.Torrents) != 1 || decoded.Torrents[0].Error != trErrorLocal || decoded.Torrents[0].ErrorString != want || decoded.Torrents[0].SeedIdleMode != transmissionLimitModeUnlimited {
		t.Fatalf("local ownership failure was hidden or removable: %+v", decoded)
	}
}
