package capture

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

func blanketHookFixture(t *testing.T) (string, string, config.Config, time.Time) {
	t.Helper()
	home, root := t.TempDir(), t.TempDir()
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	c := config.Config{MachineID: "test", Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true}}
	setTestCodexScope(&c, config.CodexAllProjects)
	if e := config.ReconcileDiscovery(&c, config.Config{}, at); e != nil {
		t.Fatal(e)
	}
	if e := config.Save(home, c); e != nil {
		t.Fatal(e)
	}
	return home, root, c, at
}

func TestBlanketDeferredFreshHookReplaysOriginalProofAndObservation(t *testing.T) {
	home, root, cfg, at := blanketHookFixture(t)
	observed := at.Add(time.Minute)
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "fresh", "cwd": root, "transcript_path": filepath.Join(root, "new.jsonl")}
	busy := func(string, time.Duration) (func(), error) { return nil, local.ErrBusy }
	if e := handleEvent(home, "codex", payload, observed, busy, nil); e != nil {
		t.Fatal(e)
	}
	old := cfg
	cfg.Archive.Projects = []archive.ProjectActivation{{Root: t.TempDir(), Included: false}}
	if e := config.ReconcileDiscovery(&cfg, old, at.Add(2*time.Minute)); e != nil {
		t.Fatal(e)
	}
	if e := config.Save(home, cfg); e != nil {
		t.Fatal(e)
	}
	if e := ReplayAdmissionIntents(home, at.Add(3*time.Minute), testDecoders); e != nil {
		t.Fatal(e)
	}
	store, _ := state.Open(home)
	regs, e := store.LoadRegistrations()
	if e != nil || len(regs) != 1 || regs[0].CodexAdmission == nil || !regs[0].SessionStartedAt.Equal(observed) || !regs[0].AdmittedAt.Equal(observed) {
		t.Fatalf("replay %#v %v", regs, e)
	}
	// Replayed continuation must keep the original proof.
	before := regs[0]
	payload["source"] = "resume"
	if e := HandleEvent(home, "codex", payload, at.Add(4*time.Minute), WithDecoders(testDecoders)); e != nil {
		t.Fatal(e)
	}
	after, _, _ := store.LoadRegistration(before.ArchiveSessionID)
	if after.CodexAdmission.Generation != before.CodexAdmission.Generation || !after.SessionStartedAt.Equal(before.SessionStartedAt) {
		t.Fatal("continuation changed admission")
	}
}

func TestBlanketHookRejectsPolicyChangesWhileWaitingAndHistoricalStarts(t *testing.T) {
	type policyChange string
	const (
		excludedChange    policyChange = "excluded"
		destinationChange policyChange = "destination"
		scopeChange       policyChange = "scope"
		pauseChange       policyChange = "pause"
	)
	for _, change := range []policyChange{excludedChange, destinationChange, scopeChange, pauseChange} {
		t.Run(string(change), func(t *testing.T) {
			home, root, cfg, at := blanketHookFixture(t)
			payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "race", "cwd": root, "transcript_path": filepath.Join(root, "new.jsonl")}
			changed := func() {
				old := cfg
				switch change {
				case excludedChange:
					cfg.Archive.Projects = []archive.ProjectActivation{{Root: root, Included: false}}
				case destinationChange:
					cfg.Storage.Bucket = "new"
				case scopeChange:
					setTestCodexScope(&cfg, config.CodexIncludedProjects)
				case pauseChange:
					cfg.Paused = true
				}
				if e := config.ReconcileDiscovery(&cfg, old, at.Add(2*time.Minute)); e != nil {
					t.Fatal(e)
				}
				if e := config.Save(home, cfg); e != nil {
					t.Fatal(e)
				}
			}
			lock := func(string, time.Duration) (func(), error) { return func() {}, nil }
			e := handleEvent(home, "codex", payload, at.Add(time.Minute), lock, changed)
			if e != nil && !errors.Is(e, state.ErrSessionNotRegistered) {
				t.Fatal(e)
			}
			store, _ := state.Open(home)
			regs, _ := store.LoadRegistrations()
			if len(regs) != 0 {
				t.Fatalf("stale policy registered %#v", regs)
			}
		})
	}
	home, root, _, at := blanketHookFixture(t)
	path := filepath.Join(root, "history.jsonl")
	_ = os.WriteFile(path, []byte("previous conversation"), 0600)
	if e := HandleEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "session_id": "old", "cwd": root, "transcript_path": path}, at.Add(time.Minute), WithDecoders(testDecoders)); e != nil {
		t.Fatal(e)
	}
	store, _ := state.Open(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 0 {
		t.Fatal("historical unknown start admitted")
	}
}

func TestBlanketContinuationRejectsUnrelatedOrUnavailableCwdAndCurrentExclusion(t *testing.T) {
	home, root, cfg, at := blanketHookFixture(t)
	_ = os.Mkdir(filepath.Join(root, ".git"), 0700)
	child := filepath.Join(root, "private")
	_ = os.Mkdir(child, 0700)
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native", "cwd": root, "transcript_path": filepath.Join(root, "new.jsonl")}
	if e := HandleEvent(home, "codex", payload, at.Add(time.Minute), WithDecoders(testDecoders)); e != nil {
		t.Fatal(e)
	}
	store, _ := state.Open(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 1 {
		t.Fatal(regs)
	}
	before := regs[0]
	for _, cwd := range []string{t.TempDir(), filepath.Join(root, "missing")} {
		for _, name := range []string{"SessionStart", "UserPromptSubmit", "Stop"} {
			payload["hook_event_name"] = name
			payload["source"] = "resume"
			payload["cwd"] = cwd
			if e := HandleEvent(home, "codex", payload, at.Add(2*time.Minute), WithDecoders(testDecoders)); e != nil {
				t.Fatal(e)
			}
			after, _, _ := store.LoadRegistration(before.ArchiveSessionID)
			if !after.HookObservedAt.Equal(before.HookObservedAt) || after.TranscriptPath != before.TranscriptPath {
				t.Fatalf("incompatible %s/%s updated identity", name, cwd)
			}
		}
	}
	old := cfg
	cfg.Archive.Projects = []archive.ProjectActivation{{Root: child, Included: false}}
	if e := config.ReconcileDiscovery(&cfg, old, at.Add(3*time.Minute)); e != nil {
		t.Fatal(e)
	}
	if e := config.Save(home, cfg); e != nil {
		t.Fatal(e)
	}
	payload["cwd"] = child
	payload["hook_event_name"] = "UserPromptSubmit"
	if e := HandleEvent(home, "codex", payload, at.Add(4*time.Minute), WithDecoders(testDecoders)); e != nil {
		t.Fatal(e)
	}
	after, _, _ := store.LoadRegistration(before.ArchiveSessionID)
	if !after.HookObservedAt.Equal(before.HookObservedAt) {
		t.Fatal("subdirectory exclusion lost rawcwd")
	}
}

func setTestCodexScope(c *config.Config, scope config.CodexCaptureScope) {
	p := config.CodexCaptureConfig{}
	if c.CodexCapture != nil {
		p = *c.CodexCapture
	}
	p.Scope = scope
	c.CodexCapture = &p
}
