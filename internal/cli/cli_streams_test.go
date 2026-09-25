package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// U-23: a command that cannot run before setup says so and leaves no data
// directory behind (show and pause used to create it first).
func TestCommandsBeforeSetupCreateNoDataDirectory(t *testing.T) {
	feedback := filepath.Join(t.TempDir(), "feedback.txt")
	must(t, os.WriteFile(feedback, []byte("good"), 0o600))
	for _, args := range [][]string{{"show", "x"}, {"list"}, {"pause"}, {"resume"}, {"sync"}, {"status"}, {"feedback", "x", "--file", feedback}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			data := filepath.Join(t.TempDir(), "agent-archive")
			t.Setenv("AGENT_ARCHIVE_HOME", data)
			env := testEnv(t, "", time.Now())
			env.Home = nil // the real resolution, from AGENT_ARCHIVE_HOME
			var out, errOut bytes.Buffer
			code := Run(args, nil, &out, &errOut, env)
			if _, err := os.Stat(data); !os.IsNotExist(err) {
				t.Fatalf("created %s (exit %d)\n%s%s", data, code, &out, &errOut)
			}
			if args[0] != "status" && (code != 1 || !strings.Contains(strings.ToLower(errOut.String()), "set up") || out.Len() != 0) {
				t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, &out, &errOut)
			}
		})
	}
}

// A home directory that is a Git checkout (a dotfiles repository) stops
// every command; the message names the checkout and the way out.
func TestGitCheckoutAboveTheDataDirectoryIsNamed(t *testing.T) {
	root := t.TempDir()
	must(t, os.Mkdir(filepath.Join(root, ".git"), 0o700))
	t.Setenv("AGENT_ARCHIVE_HOME", filepath.Join(root, ".local", "share", "agent-archive"))
	env := testEnv(t, "", time.Now())
	env.Home = nil
	var out, errOut bytes.Buffer
	if code := Run([]string{"status"}, nil, &out, &errOut, env); code != 1 {
		t.Fatalf("exit %d", code)
	}
	resolved, err := filepath.EvalSymlinks(root)
	must(t, err)
	if !strings.Contains(errOut.String(), "Git checkout "+resolved) || !strings.Contains(errOut.String(), "AGENT_ARCHIVE_HOME") {
		t.Fatalf("stderr: %s", &errOut)
	}
}

// sync says which command holds the collector lock, not "another sync";
// a paused sync, like any sync that did not run, says why on stderr and
// exits 1.
func TestSyncNamesTheLockHolderAndReportsOnStderr(t *testing.T) {
	t.Parallel()
	project, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	home, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, project))
	unlock, err := lockCollector(home, "backfill import", env.now())
	must(t, err)
	var out, errOut bytes.Buffer
	code := Run([]string{"sync"}, nil, &out, &errOut, env)
	unlock()
	want := "agent-archive backfill import (process " + strconv.Itoa(os.Getpid())
	if code != 1 || out.Len() != 0 || !strings.Contains(errOut.String(), want) {
		t.Fatalf("busy: exit %d\nstdout: %s\nstderr: %s", code, &out, &errOut)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"pause"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("pause: %d %s", code, &errOut)
	}
	out.Reset()
	errOut.Reset()
	code = Run([]string{"sync"}, nil, &out, &errOut, env)
	if code != 1 || out.Len() != 0 || !strings.Contains(errOut.String(), "agent-archive: sync: collection is paused") {
		t.Fatalf("paused: exit %d\nstdout: %s\nstderr: %s", code, &out, &errOut)
	}
}
