package cli

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/hooks"
)

// Uninstalling a test installation whose data directory is already gone (its
// temporary folder was cleaned up) removes its hooks and LaunchAgent and
// leaves no data directory behind: before, uninstall created an empty one
// (with its lock files) to hold its locks, and kept it.
func TestUninstallOfAStaleInstallationLeavesNoDataDirectory(t *testing.T) {
	for _, purge := range []bool{false, true} {
		name := "keep local data"
		if purge {
			name = "delete local data"
		}
		t.Run(name, func(t *testing.T) {
			scratch, userHome := t.TempDir(), t.TempDir()
			parent := filepath.Join(scratch, "test-install")
			home := filepath.Join(parent, "data")
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now().UTC())
			setupRun(t, env, s3SetupInput("b", "us-east-1", "p", true, true, true, t.TempDir()), 0)
			in := env.installation(home, userHome)
			executable, _ := env.executable()
			plist := in.collectorPlist()
			if _, err := os.Stat(plist); err != nil {
				t.Fatalf("setup left no LaunchAgent: %v", err)
			}
			if err := os.RemoveAll(parent); err != nil {
				t.Fatal(err)
			}

			args := []string{"uninstall", "--yes"}
			if purge {
				args = append(args, "--delete-local-data")
			}
			var out, errOut bytes.Buffer
			if code := Run(args, nil, &out, &errOut, env); code != 0 {
				t.Fatalf("uninstall exit %d:\n%s%s", code, &out, &errOut)
			}
			for _, dir := range []string{home, parent} {
				if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
					entries, _ := os.ReadDir(dir)
					t.Fatalf("uninstall left %s behind (%v): %v", dir, err, entries)
				}
			}
			if _, err := os.Stat(plist); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the LaunchAgent is still there: %v", err)
			}
			for _, app := range allHarnesses {
				if ok, err := hooks.Installed(legacyHookFiles(userHome), in.hook(executable), app); ok || err != nil {
					t.Fatalf("%s hooks still installed (%v)", app, err)
				}
			}
		})
	}
}

// A data directory that exists keeps its lock files and itself after an
// uninstall that keeps local data, as before.
func TestUninstallKeepsAnExistingDataDirectory(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now().UTC())
	setupRun(t, env, s3SetupInput("b", "us-east-1", "p", true, false, false, t.TempDir()), 0)
	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall", "--yes"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("uninstall exit %d:\n%s%s", code, &out, &errOut)
	}
	if _, err := os.Stat(filepath.Join(home, "config.json")); err != nil {
		t.Fatalf("uninstall removed kept local data: %v", err)
	}
}
