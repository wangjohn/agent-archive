//go:build unix

package host

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup starts cmd in a process group of its own, so the SIGINT,
// SIGQUIT and SIGHUP a terminal sends its foreground group (Ctrl-C, Ctrl-\,
// a closing window) do not reach it. Setup --refresh absorbs those signals while it
// changes files and restarts the job; a launchctl that a Ctrl-C killed in
// the middle would fail the restart, and a second Ctrl-C would fail the
// rollback's restart too.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
