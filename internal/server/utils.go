package server

import (
	"context"
	"fmt"

	"github.com/elsbrock/plundrio/internal/log"
)

// checkDiskQuota checks disk usage and handles quota warnings
func (s *Server) checkDiskQuota() (bool, error) {
	account, err := s.client.GetAccountInfo(context.Background())
	if err != nil {
		return false, fmt.Errorf("failed to check disk quota: %w", err)
	}

	// Calculate usage percentage
	usagePercent := float64(account.Disk.Used) / float64(account.Disk.Size) * 100

	// Consider over quota if usage is above 95%
	isOverQuota := usagePercent >= 95

	if isOverQuota && !s.quotaWarning.Load() {
		log.Warn("server").Msgf("Put.io account is over quota (%.1f%% used)", usagePercent)
		s.quotaWarning.Store(true)
	} else if !isOverQuota && s.quotaWarning.Load() {
		// Reset warning when usage drops
		s.quotaWarning.Store(false)
	}

	return isOverQuota, nil
}
