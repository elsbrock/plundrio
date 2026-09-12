//go:build unix

package reconcile

import (
	"fmt"
	"os"
	"syscall"
)

func systemIdentity(info os.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return ""
	}
	return fmt.Sprintf("%d:%d:%s", stat.Dev, stat.Ino, changeTime(stat))
}
