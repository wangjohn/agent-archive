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
	if err := HandleEvent(home, "codex", startPayload("/work/widget"), time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), WithRepoKey(lookup)); err != nil {
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
		if err := HandleEvent(home, "codex", startPayload("/work/widget"), at, lookup); err != nil {
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
// loaded the machine is.
func TestHookRegistersOnTimeWhenTheRepoKeyLookupHangs(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		start := time.Now()
		reg := registerWithRepoKey(t, func(string) string {
			<-release
			return archive.RepoKey("https://example.test/acme/widget.git")
		})
		if reg.RepoKey != "" {
			t.Errorf("RepoKey = %q, want none from a lookup that never answered", reg.RepoKey)
		}
		// A hook that waited for the lookup itself would never return: the
		// bubble would panic as deadlocked instead.
		if elapsed := time.Since(start); elapsed != repoKeyBudget {
			t.Errorf("the hook took %v with a hung lookup, want exactly the lookup's budget, %v", elapsed, repoKeyBudget)
		}
	})
}
