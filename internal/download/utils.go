package download

import (
	"fmt"
	"time"
)

// formatETA formats the estimated time remaining in a human-readable format
func formatETA(seconds int) string {
	if seconds <= 0 {
		return "unknown"
	}
	duration := time.Duration(seconds) * time.Second
	hours := int(duration.Hours())
	minutes := int(duration.Minutes()) % 60
	secs := int(duration.Seconds()) % 60

	if hours > 0 {
		return fmt.Sprintf("%dh%02dm", hours, minutes)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm%02ds", minutes, secs)
	}
	return fmt.Sprintf("%ds", secs)
}
