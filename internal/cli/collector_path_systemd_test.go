package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/scheduler/systemd"
)

// A Linux collector finds a credential helper where the shell does. systemd's
// own default PATH (what a service gets when its unit sets none: a user
// manager's own environment, in practice /usr/local/bin and the system
// directories) has neither ~/.local/bin nor any other directory under the
// home, where aws-vault, 1Password's op and pipx installs commonly live, so the
// job finds them only through the PATH setup records: this shell's usable
// entries, then systemd's default. This pins that composition with the real
// default.
func TestLinuxCollectorPathIsTheShellsFollowedBySystemdsDefault(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	localBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localBin, "op"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(home, "not-yet-installed", "bin")
	shell := strings.Join([]string{localBin, missing, "relative/bin", localBin}, ":")

	got := collectorPath(shell, systemd.DefaultPATH)
	entries := filepath.SplitList(got)
	if want := append([]string{localBin}, filepath.SplitList(systemd.DefaultPATH)...); !slices.Equal(entries, want) {
		t.Errorf("PATH = %q, want the shell's usable entry, without its repeat, then systemd's default: %q", entries, want)
	}

	// The helper in ~/.local/bin is found through the recorded PATH, and not
	// through systemd's default alone.
	if !programFound("op", got, home) {
		t.Errorf("op in ~/.local/bin is not found on the recorded PATH %q", got)
	}
	if programFound("op", systemd.DefaultPATH, home) {
		t.Errorf("systemd's default PATH %q finds a helper in ~/.local/bin", systemd.DefaultPATH)
	}
	// A directory that does not exist when setup runs is not recorded: a
	// helper installed there later is not found until setup runs again.
	if err := os.MkdirAll(missing, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(missing, "aws-vault"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if programFound("aws-vault", got, home) {
		t.Errorf("aws-vault is found on %q through a directory that was not there when it was recorded", got)
	}
}
