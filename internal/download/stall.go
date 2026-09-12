package download

import (
	"fmt"
	"time"

	"github.com/elsbrock/go-putio"
)

type transferProgress struct {
	downloaded   int64
	lastProgress time.Time
	downloading  bool
}

// stallTracker detects Put.io transfers whose downloaded byte count remains
// unchanged while their status is DOWNLOADING. COMPLETING preserves the byte
// baseline but does not report an error until DOWNLOADING resumes. Its state is intentionally
// in-memory so a restart always establishes a fresh observation baseline. It
// is owned by the monitor goroutine; RPC only sees annotated snapshots.
type stallTracker struct {
	timeout  time.Duration
	now      func() time.Time
	progress map[int64]transferProgress
}

func newStallTracker(timeout time.Duration, now func() time.Time) *stallTracker {
	return &stallTracker{
		timeout:  timeout,
		now:      now,
		progress: make(map[int64]transferProgress),
	}
}

func (s *stallTracker) Observe(transfers []*putio.Transfer) {
	if s.timeout <= 0 {
		return
	}

	now := s.now()
	active := make(map[int64]struct{}, len(transfers))
	for _, transfer := range transfers {
		if transfer.Status != "DOWNLOADING" && transfer.Status != "COMPLETING" {
			continue
		}

		active[transfer.ID] = struct{}{}
		progress, exists := s.progress[transfer.ID]
		if !exists || progress.downloaded != transfer.Downloaded {
			progress = transferProgress{
				downloaded:   transfer.Downloaded,
				lastProgress: now,
			}
		}
		progress.downloading = transfer.Status == "DOWNLOADING"
		s.progress[transfer.ID] = progress
	}

	for transferID := range s.progress {
		if _, exists := active[transferID]; !exists {
			delete(s.progress, transferID)
		}
	}
}

func (s *stallTracker) Error(transferID int64) string {
	if progress, exists := s.progress[transferID]; exists && progress.downloading && s.now().Sub(progress.lastProgress) >= s.timeout {
		return fmt.Sprintf(
			"Put.io transfer stalled: no byte progress for %s; inspect the transfer in Put.io",
			s.timeout,
		)
	}
	return ""
}
