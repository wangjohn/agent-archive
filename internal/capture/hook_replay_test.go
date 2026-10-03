package capture

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
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
			if err := HandleEvent(home, "codex", startPayload("/work/widget"), time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), WithReplay(value), WithDecoders(testDecoders)); err != nil {
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
		"another run resumes a replay":             {"run-1", "run-2"},
		"a person resumes a replay":                {"run-1", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
			if err := HandleEvent(home, "codex", startPayload("/work/widget"), at, WithReplay(tc.first), WithDecoders(testDecoders)); err != nil {
				t.Fatal(err)
			}
			resumed := startPayload("/work/widget")
			resumed["source"] = "resume"
			if err := HandleEvent(home, "codex", resumed, at.Add(time.Hour), WithReplay(tc.then), WithDecoders(testDecoders)); err != nil {
				t.Fatal(err)
			}
			regs := registrations(t, home)
			if len(regs) != 1 || (regs[0].Replay != nil) != (tc.first != "") || (regs[0].Replay != nil && regs[0].Replay.RunID != tc.first) {
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
	batch, err := testBatch("claude", claudeStart(project, "native-replay", "startup", ""), at)
	if err != nil {
		t.Fatal(err)
	}
	if err := handleBatch(home, "claude", batch, at, busy, nil, eventOptions{replay: &archive.Replay{RunID: "run-7"}, decoders: testDecoders}); err != nil {
		t.Fatalf("queued start: %v", err)
	}
	release()
	if err := ReplayAdmissionIntents(home, at.Add(2*time.Second), testDecoders); err != nil {
		t.Fatal(err)
	}
	regs := registrations(t, home)
	if len(regs) != 1 || regs[0].Replay == nil || regs[0].Replay.RunID != "run-7" {
		t.Fatalf("admitted registrations = %#v, want the replay marker kept", regs)
	}
}

// Interrupted ordered admissions retain the original hook marker across every
// durable effect and retry, even when no replay environment is present later.
func TestOrderedAdmissionKeepsReplayMarkerAfterInterruptedEffects(t *testing.T) {
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	for _, boundary := range []effectName{effectRegistrationCreate, effectLocatorUpdate, effectEvidenceSave, effectRequestSave, effectIntentAck} {
		t.Run(string(boundary), func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			setUpTestConfig(t, home, project, at.Add(-time.Hour))
			follow := syntheticLifecycle(agentapi.HookInput{Payload: syntheticPayload(signalAnswer, "marked", project, "/earlier"), ObservedAt: at})
			start := syntheticLifecycle(agentapi.HookInput{Payload: syntheticPayload(signalBegin, "marked", project, ""), ObservedAt: at})
			interrupted := false
			err := handleBatch(home, "synthetic", append(follow, start...), at, nil, nil, eventOptions{
				replay: archive.ParseReplay("run-ordered"),
				afterEffect: func(name effectName) error {
					if name == boundary && !interrupted {
						interrupted = true
						return errors.New("interrupted admission")
					}
					return nil
				},
			})
			if err == nil || !interrupted {
				t.Fatalf("expected interrupted %s: %v", boundary, err)
			}
			for range 2 {
				if err := ReplayAdmissionIntents(home, at.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			regs := registrations(t, home)
			if len(regs) != 1 || regs[0].Replay == nil || regs[0].Replay.RunID != "run-ordered" || regs[0].TranscriptPath != "/earlier" {
				t.Fatalf("admission lost replay identity or locator: %+v", regs)
			}
		})
	}
}
