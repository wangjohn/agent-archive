//go:build unix

package cli

import (
	"os"
	"os/exec"
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

// ownProcessGroup starts cmd in a process group of its own, so the SIGINT
// and SIGHUP a terminal sends its foreground group (Ctrl-C, a closing
// window) do not reach it. Setup --refresh absorbs those signals while it
// changes files and restarts the job; a launchctl that a Ctrl-C killed in
// the middle would fail the restart, and a second Ctrl-C would fail the
// rollback's restart too.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
