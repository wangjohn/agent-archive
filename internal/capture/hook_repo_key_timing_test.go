package capture

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

func startPayload(root string) map[string]any {
	return map[string]any{
		"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1",
		"cwd": root, "transcript_path": "/tmp/t.jsonl",
	}
}

// The lookup runs before hooks.lock is taken: a slow lookup must not hold
// the lock other hooks and the collector wait for.
func TestHookAsksForTheRepoKeyBeforeTakingTheLock(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	asked := 0
	lookup := func(string) string {
		asked++
		unlock, err := local.NamedLockWait(home, "hooks.lock", 50*time.Millisecond)
		if err != nil {
			t.Errorf("hooks.lock was held while the lookup ran: %v", err)
			return ""
		}
		unlock()
		return archive.RepoKey("https://example.test/acme/widget.git")
	}
	if err := HandleEvent(home, "codex", startPayload("/work/widget"), time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), WithRepoKey(lookup), WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	if asked != 1 {
		t.Errorf("the lookup ran %d times, want 1", asked)
	}
	store, _ := state.Open(home)
	if regs, _ := store.LoadRegistrations(); len(regs) != 1 || regs[0].RepoKey == "" {
		t.Fatalf("registrations = %#v, want one with a key", regs)
	}
}

func TestHookDoesNotAskAgainForAContinuationOfARegisteredSession(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	asked := 0
	lookup := WithRepoKey(func(string) string { asked++; return "" })
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for range 3 {
		if err := HandleEvent(home, "codex", startPayload("/work/widget"), at, lookup, WithDecoders(testDecoders)); err != nil {
			t.Fatal(err)
		}
	}
	if asked != 1 {
		t.Errorf("the lookup ran %d times for one session, want 1", asked)
	}
}

// A lookup that hangs (a stalled mount) costs the hook its budget and no
// more, and the session still registers, without a key. The hook runs in a
// synctest bubble, whose clock moves only while every goroutine in it is
// blocked: the file reads and writes around the lookup take no time on it, so
// the hook's time on that clock is what it waited for the lookup, however
// loaded the machine is. By the same token, this does not see a hook slowed
// by real I/O or a program it runs; only timers and sleeps move the clock.
func TestHookRegistersOnTimeWhenTheRepoKeyLookupHangs(t *testing.T) {
	t.Parallel()
	wall := time.Now()
	synctest.Test(t, func(t *testing.T) {
		// The bubble's clock starts in 2000; bring it to the real time, so
		// anything the hook compares with a file's mtime sees what it would
		// outside the bubble.
		time.Sleep(time.Until(wall))
		// A hook that waited for the lookup itself would deadlock the bubble,
		// which panics the whole test binary. The lookup answers after a
		// minute instead, and the checks below fail as usual.
		release := make(chan struct{})
		answer := time.AfterFunc(time.Minute, func() { close(release) })
		t.Cleanup(func() {
			if answer.Stop() {
				close(release)
			}
		})
		start := time.Now()
		reg := registerWithRepoKey(t, func(string) string {
			<-release
			return archive.RepoKey("https://example.test/acme/widget.git")
		})
		if reg.RepoKey != "" {
			t.Errorf("RepoKey = %q, want none from a lookup that never answered", reg.RepoKey)
		}
		// Anything but the budget is a changed budget or the hook waiting on
		// a timer or sleep besides the lookup's.
		elapsed := time.Since(start)
		if elapsed != repoKeyBudget {
			t.Errorf("the hook took %v on timers and sleeps with a hung lookup, want exactly the lookup's budget, %v", elapsed, repoKeyBudget)
		}
		// With the lock wait left after it, the lookup still fits in the 2s
		// timeout hooks.Merge gives every hook.
		if total := elapsed + lockWaitAfter(elapsed); total >= 2*time.Second {
			t.Errorf("the lookup's %v and the lock wait after it take %v, want under the 2s hook timeout", elapsed, total)
		}
	})
}
