//go:build unix

package cli

import (
	"os"
	"syscall"
)

// privateNativeTempDirectory checks ownership on the same non-followed file
// information used to reject symlinks and public permissions.
func privateNativeTempDirectory(info os.FileInfo, uid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.IsDir() && info.Mode().Perm()&0o077 == 0 && int(stat.Uid) == uid
}
