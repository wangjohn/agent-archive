//go:build unix

package cli

import (
	"os"
	"syscall"
)

// fileOwner is the user ID that owns path; ok is false when it cannot be
// read.
func fileOwner(path string) (uid int, ok bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	st, isStat := info.Sys().(*syscall.Stat_t)
	if !isStat {
		return 0, false
	}
	return int(st.Uid), true
}
