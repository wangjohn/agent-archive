package capture

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestHookLockTimeoutLeavesContentFreeDiagnostic(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	release, err := local.NamedLock(home, "hooks.lock")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	payload := claudeStart(project, "secret-session-id", "startup", "/private/secret-transcript.jsonl")
	payload["last_assistant_message"] = "secret conversation text"
	// Only the wait for hooks.lock is timed. The admission intent and
	// diagnostic written after it are synchronous filesystem writes, which a
	// loaded runner can stretch past a second on their own.
	var asked, waited time.Duration
	var lockErr error
	var attempts int
	lock := func(home string, wait time.Duration) (func(), error) {
		attempts++
		asked = wait
		start := time.Now()
		unlock, err := local.NamedLockWait(home, "hooks.lock", wait)
		waited, lockErr = time.Since(start), err
		return unlock, err
	}
	err = handleEvent(home, "claude", payload, at, lock, nil, nil)
	if err != nil {
		t.Fatalf("queued first start: %v", err)
	}
	if attempts != 1 || !errors.Is(lockErr, local.ErrBusy) {
		t.Fatalf("hooks.lock attempts = %d, last error %v; want one that found it busy", attempts, lockErr)
	}
	// With no repository-key lookup the hook asks for the whole configured
	// wait, less the microseconds between reading the clock twice.
	if asked > hooksLockWait || asked < hooksLockWait-100*time.Millisecond {
		t.Fatalf("hook asked to wait %s for hooks.lock, want %s", asked, hooksLockWait)
	}
	// It really waited that long before giving up, and the wait alone stays
	// well inside the two-second timeout hooks.Merge gives every hook.
	if waited < asked || waited >= 2*time.Second {
		t.Fatalf("hook waited %s for hooks.lock, want at least %s and under the 2s hook timeout", waited, asked)
	}
	diagnostics, err := ReadDiagnostics(home)
	if err != nil || len(diagnostics) != 1 || diagnostics[0].Code != DiagnosticHookBusy || diagnostics[0].ProjectRoot != project || !diagnostics[0].ObservedAt.Equal(at) {
		t.Fatalf("diagnostics = %#v, %v", diagnostics, err)
	}
	raw, err := os.ReadFile(DiagnosticsPath(home))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-session-id", "secret-transcript", "secret conversation text"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("diagnostic contains %q: %s", secret, raw)
		}
	}
	release()
	if err := ReplayAdmissionIntents(home, at.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	store := state.OpenReadOnly(home)
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 1 || regs[0].NativeSessionID != "secret-session-id" {
		t.Fatalf("replayed registrations = %#v, %v", regs, err)
	}
}

func TestHookLockAndDiagnosticsLockTimeoutExplainsMissingStatus(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	releaseHook, err := local.NamedLock(home, "hooks.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseHook()
	releaseDiagnostics, err := local.NamedLock(home, DiagnosticsLockName)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDiagnostics()
	err = HandleEvent(home, "claude", claudeStart(project, "session", "startup", ""), time.Now())
	if err == nil || !strings.Contains(err.Error(), "status may not show") {
		t.Fatalf("error = %v", err)
	}
	if diagnostics, err := ReadDiagnostics(home); err != nil || len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v, %v", diagnostics, err)
	}
}
