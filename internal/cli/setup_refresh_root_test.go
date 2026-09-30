package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

// Run as root (sudo keeps HOME), refresh would write root-owned hook files,
// skills, the plist, the journal and the locks into another user's home, so
// it refuses, changing nothing. Root's own home, or any other user, is fine.
func TestRefreshRefusesAsRootInAnotherUsersHome(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		euid    int
		owner   func(path string) (int, bool)
		refused bool
	}{
		{"root in a user's home", 0, func(string) (int, bool) { return 501, true }, true},
		{"root in root's own home", 0, func(string) (int, bool) { return 0, true }, false},
		{"root, owner unknown", 0, func(string) (int, bool) { return 0, false }, false},
		{"a user in their own home", 501, func(string) (int, bool) { return 501, true }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRefreshFixture(t, "loaded")
			f.env.EffectiveUID = func() int { return tc.euid }
			f.env.FileOwner = tc.owner
			before, homeBefore := tree(t, f.userHome), tree(t, f.home)
			f.launchd.calls = nil
			code, stdout, stderr := refreshRun(t, f.env)
			if !tc.refused {
				if code != 0 {
					t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
				}
				return
			}
			if code != 1 || stdout != "" || !strings.Contains(stderr, "running as root, but") || !strings.Contains(stderr, "without sudo") || !strings.Contains(stderr, "Nothing was changed") || strings.Count(stderr, "\n") != 1 {
				t.Fatalf("exit %d\n%q\n%q", code, stdout, stderr)
			}
			if changed := differences(before, tree(t, f.userHome)); len(changed) != 0 {
				t.Errorf("a refused refresh changed %v", changed)
			}
			if changed := differences(homeBefore, tree(t, f.home)); len(changed) != 0 {
				t.Errorf("a refused refresh changed %v", changed)
			}
			if calls := f.launchd.all(); len(calls) != 0 {
				t.Errorf("launchd: %v", calls)
			}
		})
	}
}

// launchctl bootstrap and bootout end on their own: refresh absorbs Ctrl-C
// and SIGTERM while it runs them, so a launchctl that hangs must not leave a
// process only SIGKILL stops.
func TestLaunchctlChangesAreBounded(t *testing.T) {
	previous := launchctlChangeTimeout
	launchctlChangeTimeout = 20 * time.Millisecond
	t.Cleanup(func() { launchctlChangeTimeout = previous })
	plist := filepath.Join(t.TempDir(), "com.example.collector.plist")
	stubLaunchctlContext(t, func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == "print" {
			return []byte("path = " + plist + "\nstate = running\n"), nil
		}
		<-ctx.Done() // a hung bootstrap or bootout
		return nil, ctx.Err()
	})
	for name, run := range map[string]func(string) error{"bootstrap": loadLaunchAgent, "bootout": unloadLaunchAgent} {
		done := make(chan error, 1)
		go func() { done <- run(plist) }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "launchctl "+name) {
				t.Errorf("%s: %v", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s hung: nothing bounds launchctl", name)
		}
	}
}

// A hook holds hooks.lock for milliseconds while it registers a session; an
// installer that upgraded meanwhile waits for it rather than failing.
func TestRefreshWaitsForAHookToFinish(t *testing.T) {
	t.Parallel()
	f := newRefreshFixture(t, "loaded")
	f.env.RefreshCollectorWait = func() time.Duration { return time.Minute }
	unlock, err := local.NamedLock(f.home, "hooks.lock")
	must(t, err)
	go func() {
		time.Sleep(50 * time.Millisecond)
		unlock()
	}()
	if code, stdout, stderr := refreshRun(t, f.env); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	f.wantRunning(t, f.newExe)
}

// stubLaunchctlContext is stubLaunchctl for a stand-in that watches the
// command's context.
func stubLaunchctlContext(t *testing.T, run func(ctx context.Context, args ...string) ([]byte, error)) {
	t.Helper()
	previous := runLaunchctl
	runLaunchctl = run
	t.Cleanup(func() { runLaunchctl = previous })
}
