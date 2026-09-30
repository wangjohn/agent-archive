//go:build unix

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A launchctl that is killed when its context ends must not keep the caller
// waiting on its output: a child it left holding the pipe would otherwise
// hold execLaunchctl until it exited. The stand-in starts a sleep that lives
// far longer than the test and then hangs itself; execLaunchctl gives up
// (cmd.WaitDelay, two seconds) after the context kills the shell, though the
// sleep still has the pipe. This waits those two seconds for real: it is the
// only way to see the delay.
func TestExecLaunchctlDoesNotWaitForAChildThatOutlivesTheKill(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	stub := "#!/bin/sh\nsleep 20 &\necho $! > '" + pidFile + "'\nsleep 20\n"
	if err := os.WriteFile(filepath.Join(dir, "launchctl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	t.Cleanup(func() {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := execLaunchctl(ctx, "bootout", "gui/1/job")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("a launchctl the context killed reported success")
	}
	if elapsed > 10*time.Second {
		t.Errorf("execLaunchctl waited %v for a child holding its pipe, want about two seconds after the context ended", elapsed)
	}
	if elapsed < time.Second {
		t.Errorf("execLaunchctl returned after %v, before the child's pipe could have been given up on", elapsed)
	}
}
