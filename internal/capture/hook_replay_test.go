package capture

import (
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

func registrations(t *testing.T, home string) []archive.SessionRegistration {
	t.Helper()
	regs, err := state.OpenReadOnly(home).LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	return regs
}

// A start whose hook has AGENT_ARCHIVE_REPLAY set registers as a replay,
// with the run's identifier; one without registers as an ordinary session.
func TestHookMarksASessionStartedWithTheReplayVariable(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]*archive.Replay{
		"":                  nil,
		"run-42":            {RunID: "run-42"},
		"fix the login bug": {},
	} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			if err := HandleEvent(home, "codex", startPayload("/work/widget"), time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), WithReplay(value)); err != nil {
				t.Fatal(err)
			}
			regs := registrations(t, home)
			if len(regs) != 1 {
				t.Fatalf("registrations = %#v", regs)
			}
			got := regs[0].Replay
			if (got == nil) != (want == nil) || (got != nil && *got != *want) {
				t.Errorf("Replay = %+v, want %+v", got, want)
			}
		})
	}
}

// The marker is set once: a replay tool resuming a person's session does not
// make it a replay, and a person resuming a replay does not make it theirs.
func TestHookNeverChangesTheReplayMarkerOfARegisteredSession(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		first string
		then  string
	}{
		"a replay tool resumes a person's session": {"", "run-1"},
		"a person resumes a replay":                {"run-1", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
			if err := HandleEvent(home, "codex", startPayload("/work/widget"), at, WithReplay(tc.first)); err != nil {
				t.Fatal(err)
			}
			resumed := startPayload("/work/widget")
			resumed["source"] = "resume"
			if err := HandleEvent(home, "codex", resumed, at.Add(time.Hour), WithReplay(tc.then)); err != nil {
				t.Fatal(err)
			}
			regs := registrations(t, home)
			if len(regs) != 1 || (regs[0].Replay != nil) != (tc.first != "") {
				t.Errorf("registrations = %#v, want the marker as first registered", regs)
			}
		})
	}
}

// A replay's start that found hooks.lock busy is queued with its marker, so
// the session the collector admits later is still a replay.
func TestAQueuedReplayStartIsAdmittedAsAReplay(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	release, err := local.NamedLock(home, "hooks.lock")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	busy := func(home string, _ time.Duration) (func(), error) {
		return local.NamedLockWait(home, "hooks.lock", 10*time.Millisecond)
	}
	if err := handleEvent(home, "claude", claudeStart(project, "native-replay", "startup", ""), at, busy, nil, eventOptions{replay: &archive.Replay{RunID: "run-7"}}); err != nil {
		t.Fatalf("queued start: %v", err)
	}
	release()
	if err := ReplayAdmissionIntents(home, at.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	regs := registrations(t, home)
	if len(regs) != 1 || regs[0].Replay == nil || regs[0].Replay.RunID != "run-7" {
		t.Fatalf("admitted registrations = %#v, want the replay marker kept", regs)
	}
}
