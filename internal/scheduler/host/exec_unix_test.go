//go:build unix

package host

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// launchctl runs in a process group of its own: a Ctrl-C that reaches this
// process's group (which setup --refresh absorbs while it restarts the job)
// must not also kill the launchctl that is restarting it.
func TestExecRunsInItsOwnProcessGroup(t *testing.T) {
	dir := t.TempDir()
	stub := "#!/bin/sh\nps -o pgid= -p $$\n"
	if err := os.WriteFile(filepath.Join(dir, "launchctl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	out, err := Exec(context.Background(), "launchctl", "print")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	pgid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("the stub said %q", out)
	}
	if pgid == syscall.Getpgrp() {
		t.Errorf("launchctl ran in this process's group %d, where a terminal's Ctrl-C reaches it", pgid)
	}
}

// Exec returns what a program wrote to standard error with what it wrote to
// standard output, as the launchd adapter reads launchctl's words from them.
func TestExecReturnsCombinedOutput(t *testing.T) {
	dir := t.TempDir()
	stub := "#!/bin/sh\necho out\necho err >&2\nexit 3\n"
	if err := os.WriteFile(filepath.Join(dir, "tool"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	out, err := Exec(context.Background(), "tool")
	if err == nil {
		t.Fatal("a program that exited 3 reported success")
	}
	if got := string(out); !strings.Contains(got, "out\n") || !strings.Contains(got, "err\n") {
		t.Errorf("output %q lacks a stream", got)
	}
}
