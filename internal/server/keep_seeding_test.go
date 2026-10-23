package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/config"
	"github.com/elsbrock/plundrio/internal/download"
)

// seedIdleForProcessedTransfer reports the seed-idle fields torrent-get
// returns for a locally processed transfer whose Put.io status is putioStatus.
func seedIdleForProcessedTransfer(t *testing.T, keepSeeding bool, putioStatus string) torrentGetRPCTorrent {
	t.Helper()
	ctx := download.NewTransferContext(1, 1, download.TransferLifecycleProcessed)
	ctx.SetTotalSize(1000)
	ctx.AddDownloadedBytes(1000)
	service := &torrentAddDownloadService{
		transfers: []*putio.Transfer{{ID: 1, Hash: "processed", Status: putioStatus, PercentDone: 100, Size: 1000}},
		contexts:  map[int64]*download.TransferContext{1: ctx},
	}
	server := &Server{
		cfg:       &config.Config{TargetDir: t.TempDir(), KeepSeeding: keepSeeding},
		dlService: service,
	}
	return singleTorrentGet(t, server)
}

// singleTorrentGet sends torrent-get to server and returns the seed fields of
// the only torrent in the response.
func singleTorrentGet(t *testing.T, server *Server) torrentGetRPCTorrent {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/transmission/rpc", bytes.NewBufferString(`{"method":"torrent-get","arguments":{"fields":["status","seedIdleMode","secondsSeeding"]}}`))
	request.Header.Set("X-Transmission-Session-Id", "123")
	recorder := httptest.NewRecorder()

	server.handleRPC(recorder, request)

	var response torrentGetRPCResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || response.Result != "success" || len(response.Arguments.Torrents) != 1 {
		t.Fatalf("unexpected RPC response: %d %s", recorder.Code, recorder.Body.String())
	}
	return response.Arguments.Torrents[0]
}

func TestTorrentGetKeepSeedingWaitsForPutioToFinishSeeding(t *testing.T) {
	tests := []struct {
		name               string
		keepSeeding        bool
		putioStatus        string
		wantIdleMode       int
		wantSecondsSeeding int64
	}{
		// The default behavior does not change: the cleanup hook already deleted
		// the source, so a processed transfer is removable whatever Put.io reports.
		{name: "default seeding", keepSeeding: false, putioStatus: "SEEDING", wantIdleMode: transmissionLimitModeSingle, wantSecondsSeeding: 1},
		{name: "default completed", keepSeeding: false, putioStatus: "COMPLETED", wantIdleMode: transmissionLimitModeSingle, wantSecondsSeeding: 1},
		// With keep-seeding, removal ends seeding. So plundrio offers removal
		// only when Put.io reports the transfer COMPLETED.
		{name: "keep-seeding still seeding", keepSeeding: true, putioStatus: "SEEDING", wantIdleMode: transmissionLimitModeUnlimited, wantSecondsSeeding: 0},
		{name: "keep-seeding completed", keepSeeding: true, putioStatus: "COMPLETED", wantIdleMode: transmissionLimitModeSingle, wantSecondsSeeding: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := seedIdleForProcessedTransfer(t, tt.keepSeeding, tt.putioStatus)
			if got.Status != trStatusSeed {
				t.Errorf("status = %d, want %d (seed)", got.Status, trStatusSeed)
			}
			if got.SeedIdleMode != tt.wantIdleMode || got.SecondsSeeding != tt.wantSecondsSeeding {
				t.Errorf("seedIdleMode=%d secondsSeeding=%d, want %d and %d",
					got.SeedIdleMode, got.SecondsSeeding, tt.wantIdleMode, tt.wantSecondsSeeding)
			}
		})
	}
}
