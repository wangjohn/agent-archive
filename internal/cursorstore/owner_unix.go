//go:build unix

package cursorstore

import (
	"os"
	"syscall"
)

// ownedByCurrentUser reports whether info is owned by this process's user.
func ownedByCurrentUser(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
