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
)

// launchctl runs in a process group of its own: a Ctrl-C that reaches this
// process's group (which setup --refresh absorbs while it restarts the job)
// must not also kill the launchctl that is restarting it.
func TestLaunchctlRunsInItsOwnProcessGroup(t *testing.T) {
	dir := t.TempDir()
	stub := "#!/bin/sh\nps -o pgid= -p $$\n"
	if err := os.WriteFile(filepath.Join(dir, "launchctl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	out, err := execLaunchctl(context.Background(), "print")
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
