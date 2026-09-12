package server

import (
	"context"
	"fmt"

	"github.com/elsbrock/plundrio/internal/download"
)

// A warning without a download-failure signal. Never claim local ownership or
// completion, including during startup before the monitor restores a context.
func applyReviewStatus(info map[string]interface{}, size int, includeFiles bool) {
	info["status"] = trStatusStopped
	info["percentDone"] = 0.5
	info["leftUntilDone"] = max(int64(size), 1)
	info["rateDownload"] = 0
	info["rateUpload"] = 0
	info["eta"] = -1
	info["error"] = false // #50's integer wire-format commit changes this to 0.
	info["errorString"] = download.NeedsReviewMessage
	info["plundrioState"] = "needs-review"
	if includeFiles {
		info["files"] = []transmissionFile{}
	}
}

// Retirement removes only the remote transfer record after explicit operator
// acknowledgment and manager revalidation. It never invokes either file deleter.
func (s *Server) retireReviewedTransfer(ctx context.Context, id torrentID) (interface{}, error) {
	transfer, err := s.findTransfer(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := s.dlService.PrepareReviewRetirement(ctx, transfer); err != nil {
		return nil, fmt.Errorf("prepare reviewed retirement: %w", err)
	}
	for range 3 {
		err = s.client.DeleteTransfer(ctx, transfer.ID)
		if err == nil || ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("retire record %d: %w; review/removal hold retained, retry the same explicit retirement request", transfer.ID, err)
	}
	s.dlService.RemoveCategory(transfer.ID)
	s.dlService.RemoveTransfer(transfer.ID)
	return struct{}{}, nil
}
