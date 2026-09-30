//go:build unix

package credentials

import (
	"errors"
	"os"
	"syscall"
)

// ownedByUser reports whether info is owned by user uid.
func ownedByUser(info os.FileInfo, uid int) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == uid
}

// openNoFollow opens path for reading without following a symbolic link in
// its last element, and without blocking on a named pipe put there.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: inside the credentials folder, a validated name.
}

// isSymlinkLoop reports whether err is what opening a symbolic link with
// O_NOFOLLOW returns.
func isSymlinkLoop(err error) bool { return errors.Is(err, syscall.ELOOP) }
