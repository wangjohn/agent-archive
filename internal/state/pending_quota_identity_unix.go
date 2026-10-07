//go:build darwin || linux

package state

import (
	"fmt"
	"os"
	"syscall"
)

func pendingQuotaIdentity(info os.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
}
