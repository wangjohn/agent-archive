//go:build unix

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/scheduler/host"
)

// A launchctl that is killed when its context ends must not keep the caller
// waiting on its output: a child it left holding the pipe would otherwise
// hold host.Exec until it exited. The stand-in starts a sleep that lives
// far longer than the test and then hangs itself; host.Exec gives up
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
				// Both sleeps are in the stand-in's own process group.
				if group, err := syscall.Getpgid(pid); err == nil && group > 1 && group != syscall.Getpgrp() {
					_ = syscall.Kill(-group, syscall.SIGKILL)
				}
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	// The context ends once the stand-in has started its child, however long a
	// loaded machine takes to get there, and the wait is timed from then.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var cancelled atomic.Int64
	go func() {
		for ctx.Err() == nil {
			if data, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(data), "\n") {
				cancelled.Store(time.Now().UnixNano())
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	type result struct {
		err error
		at  time.Time
	}
	done := make(chan result, 1)
	go func() {
		_, err := host.Exec(ctx, "launchctl", "bootout", "gui/1/job")
		done <- result{err, time.Now()}
	}()
	var got result
	select {
	case got = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("host.Exec never returned")
	}
	if got.err == nil {
		t.Fatal("a launchctl the context killed reported success")
	}
	if cancelled.Load() == 0 {
		t.Fatalf("host.Exec returned (%v) before the stand-in started its child", got.err)
	}
	// Without WaitDelay it would wait for the child, 20 s; with it, 2 s. The
	// bounds leave a loaded runner room on both sides.
	elapsed := got.at.Sub(time.Unix(0, cancelled.Load()))
	if elapsed > 10*time.Second {
		t.Errorf("host.Exec waited %v for a child holding its pipe, want about two seconds after the context ended", elapsed)
	}
	if elapsed < time.Second {
		t.Errorf("host.Exec returned %v after the context ended, before the child's pipe could have been given up on", elapsed)
	}
}
