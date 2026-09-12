//go:build unix && !darwin && !freebsd && !netbsd

package reconcile

import (
	"fmt"
	"syscall"
)

func changeTime(stat *syscall.Stat_t) string {
	return fmt.Sprintf("%d:%d", stat.Ctim.Sec, stat.Ctim.Nsec)
}
