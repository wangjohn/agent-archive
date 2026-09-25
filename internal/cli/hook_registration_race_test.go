package cli

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

// assertIndexConsistent checks the invariant the races below could break:
// every registration is reachable from its native session's index entry, so
// the session's next start finds it instead of minting a second archive ID.
func assertIndexConsistent(t *testing.T, home string) {
	t.Helper()
	store := state.OpenReadOnly(home)
	regs, err := store.LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, reg := range regs {
		id, found, err := store.ArchiveSessionID(reg.NativeSessionID)
		if err != nil {
			t.Fatal(err)
		}
		if !found || id != reg.ArchiveSessionID {
			t.Fatalf("registration %s is not reachable from its native session's index (found=%t id=%q)", reg.ArchiveSessionID, found, id)
		}
		if other, dup := seen[reg.NativeSessionID]; dup {
			t.Fatalf("native session has two registrations: %s and %s", other, reg.ArchiveSessionID)
		}
		seen[reg.NativeSessionID] = reg.ArchiveSessionID
	}
}

func claudeStart(project, native, source, transcript string) map[string]any {
	return map[string]any{"hook_event_name": "SessionStart", "source": source, "session_id": native, "cwd": project, "transcript_path": transcript}
}

// Retention holds the session's request lock while it forgets the session. A
// resume that arrives at that instant waits for the lock, then finds the
// registration gone and treats the session as never seen: a resume is not a
// provable fresh start, so nothing is registered and the reason is recorded.
// Before, the resume wrote the registration back without its index entry.
func TestResumeDuringExpiryIsTreatedAsNeverSeen(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	transcript := writeTestTranscript(t, "t.jsonl", "")
	if err := handleHookEvent(home, "claude", claudeStart(project, "native-1", "startup", transcript), at); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	archiveID, found, err := store.ArchiveSessionID("native-1")
	if err != nil || !found {
		t.Fatalf("setup: %v", err)
	}

	// Retention takes the lock and starts forgetting.
	release, err := local.NamedLock(home, filepath.Join("request-locks", archiveID+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	resumed := make(chan error, 1)
	go func() {
		resumed <- handleHookEvent(home, "claude", claudeStart(project, "native-1", "resume", transcript), at.Add(90*24*time.Hour))
	}()
	time.Sleep(100 * time.Millisecond) // the resume is now waiting for the lock
	if err := store.ForgetSession(archiveID, "native-1"); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-resumed; err != nil {
		t.Fatalf("resume: %v", err)
	}

	assertIndexConsistent(t, home)
	if regs, _ := store.LoadRegistrations(); len(regs) != 0 {
		t.Fatalf("a forgotten session was written back: %#v", regs)
	}
	if ds, _ := readCaptureDiagnostics(home); len(ds) != 1 || ds[0].Code != diagnosticUnknownSessionStart {
		t.Fatalf("the declined resume was not explained: %#v", ds)
	}
}

// The same race for a start that is provably fresh (a Claude Code /clear keeps
// the native session): it registers again, under a fresh archive ID that its
// index entry points at.
func TestFreshStartDuringExpiryRegistersUnderAFreshID(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	if err := handleHookEvent(home, "claude", claudeStart(project, "native-1", "startup", writeTestTranscript(t, "t.jsonl", "")), at); err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(home)
	oldID, _, _ := store.ArchiveSessionID("native-1")
	release, err := local.NamedLock(home, filepath.Join("request-locks", oldID+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan error, 1)
	go func() {
		started <- handleHookEvent(home, "claude", claudeStart(project, "native-1", "clear", writeTestTranscript(t, "t2.jsonl", "")), at.Add(90*24*time.Hour))
	}()
	time.Sleep(100 * time.Millisecond)
	if err := store.ForgetSession(oldID, "native-1"); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	assertIndexConsistent(t, home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 1 || regs[0].ArchiveSessionID == oldID {
		t.Fatalf("registrations = %#v (old ID %s)", regs, oldID)
	}
}

// Whatever the interleaving of a resume (or /clear) with retention forgetting
// the session, no registration is ever left unreachable from its index.
func TestConcurrentStartAndForgetKeepTheIndexConsistent(t *testing.T) {
	// Not parallel: the hook waits a bounded time for the lock the forget
	// holds, which a machine busy with the parallel tests' I/O can exceed.
	const rounds = 30
	for round := range rounds {
		home, project := t.TempDir(), t.TempDir()
		setUpTestConfig(t, home, project, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
		native := fmt.Sprintf("native-%d", round)
		if err := handleHookEvent(home, "claude", claudeStart(project, native, "startup", writeTestTranscript(t, "t.jsonl", "")), at); err != nil {
			t.Fatal(err)
		}
		store, _ := state.Open(home)
		archiveID, _, _ := store.ArchiveSessionID(native)
		source := "resume"
		if round%2 == 1 {
			source = "clear"
		}
		payload := claudeStart(project, native, source, writeTestTranscript(t, "t2.jsonl", ""))

		var wg sync.WaitGroup
		var hookErr, forgetErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, forgetErr = store.ForgetIdleSession(archiveID, native, false, nil)
		}()
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(round%10) * 50 * time.Microsecond)
			hookErr = handleHookEvent(home, "claude", payload, at.Add(time.Hour))
		}()
		wg.Wait()
		if forgetErr != nil || hookErr != nil {
			t.Fatalf("round %d: forget=%v hook=%v", round, forgetErr, hookErr)
		}
		assertIndexConsistent(t, home)
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestHookWaitsForOverlappingRegistration(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	now := time.Now()
	setUpTestConfig(t, home, dir, now.Add(-time.Hour))
	unlock, err := local.NamedLock(home, "hooks.lock")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- handleHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "overlap", "cwd": dir}, now)
	}()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("hook did not wait: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	regs, err := state.OpenReadOnly(home).LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("regs=%+v err=%v", regs, err)
	}
}
