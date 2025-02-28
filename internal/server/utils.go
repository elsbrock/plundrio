package server

import (
	"fmt"

	"github.com/elsbrock/plundrio/internal/log"
)

// mapPutioStatus converts Put.io transfer status to transmission status
func (s *Server) mapPutioStatus(status string) int {
	switch status {
	case "IN_QUEUE":
		return 3 // TR_STATUS_DOWNLOAD_WAITING
	case "DOWNLOADING":
		return 4 // TR_STATUS_DOWNLOAD
	case "COMPLETING":
		return 4 // TR_STATUS_DOWNLOAD
	case "SEEDING":
		return 6 // TR_STATUS_SEED
	case "COMPLETED":
		return 6 // TR_STATUS_SEED
	case "ERROR":
		return 0 // TR_STATUS_STOPPED
	default:
		return 0 // TR_STATUS_STOPPED
	}
}

// checkDiskQuota checks disk usage and handles quota warnings
func (s *Server) checkDiskQuota() (bool, error) {
	account, err := s.client.GetAccountInfo()
	if err != nil {
		return false, fmt.Errorf("failed to check disk quota: %w", err)
	}

	// Calculate usage percentage
	usagePercent := float64(account.Disk.Used) / float64(account.Disk.Size) * 100

	// Consider over quota if usage is above 95%
	isOverQuota := usagePercent >= 95

	if isOverQuota && !s.quotaWarning {
		log.Warn("server").Msgf("Put.io account is over quota (%.1f%% used)", usagePercent)
		s.quotaWarning = true
	} else if !isOverQuota && s.quotaWarning {
		// Reset warning when usage drops
		s.quotaWarning = false
	}

	return isOverQuota, nil
}
