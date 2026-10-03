package capture

import (
	"errors"
	"fmt"
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

func TestBlanketReplayRotatesProjectBudgetPastPersistentlyInterruptedIntents(t *testing.T) {
	home, root, _, at := blanketHookFixture(t)
	for range 20 {
		root = filepath.Join(root, "deep")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	busy := func(string, time.Duration) (func(), error) { return nil, local.ErrBusy }
	for i := range maxAdmissionIntents {
		cwd := filepath.Join(root, fmt.Sprintf("project-%03d", i))
		if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0700); err != nil {
			t.Fatal(err)
		}
		payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": fmt.Sprintf("native-%03d", i), "cwd": cwd}
		if err := handleEvent(home, "codex", payload, at.Add(time.Minute+time.Duration(i)), busy, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(queueFiles(t, home)); got != maxAdmissionIntents {
		t.Fatalf("queued %d intents", got)
	}
	if err := os.WriteFile(filepath.Join(home, "admission-replay-cursor.json"), []byte("{\"after\":\"../untrusted\"}"), 0600); err != nil {
		t.Fatal(err)
	}
	interrupted := errors.New("synthetic persistent evidence interruption")
	for range 8 {
		err := replayAdmissionIntents(home, at.Add(2*time.Minute), testDecoders, func(effect effectName) error {
			if effect == effectEvidenceSave {
				return interrupted
			}
			return nil
		})
		if err != nil && !errors.Is(err, interrupted) {
			t.Fatal(err)
		}
	}
	store := state.OpenReadOnly(home)
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != maxAdmissionIntents {
		t.Fatalf("persistent leaders starved registrations: got %d, error %v", len(regs), err)
	}
	if got := len(queueFiles(t, home)); got != maxAdmissionIntents {
		t.Fatalf("interrupted original intents were lost: %d", got)
	}
	for _, reg := range regs {
		if reg.CodexAdmission == nil || reg.CodexAdmission.Cwd != reg.ProjectRoot || !reg.AdmittedAt.Equal(reg.SessionStartedAt) || !reg.AdmittedAt.Before(at.Add(2*time.Minute)) {
			t.Fatalf("replay changed original proof/time: %#v", reg)
		}
	}
	for range 8 {
		if err := ReplayAdmissionIntents(home, at.Add(3*time.Minute), testDecoders); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(queueFiles(t, home)); got != 0 {
		t.Fatalf("completed intents remain: %d", got)
	}
	final, err := store.LoadRegistrations()
	if err != nil || len(final) != len(regs) {
		t.Fatalf("duplicate/lost registration: %d, %v", len(final), err)
	}
}

func TestBlanketReplayPrunesIneligibleIntentsEvenWhenCwdDisappears(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(fmt.Sprintf("revoked-%t", revoked), func(t *testing.T) {
			home, root, cfg, at := blanketHookFixture(t)
			busy := func(string, time.Duration) (func(), error) { return nil, local.ErrBusy }
			payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native", "cwd": root}
			if err := handleEvent(home, "codex", payload, at.Add(time.Minute), busy, nil); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(root); err != nil {
				t.Fatal(err)
			}
			now := at.Add(25 * time.Hour)
			if revoked {
				old := cfg
				cfg.Archive.Projects = []archive.ProjectActivation{{Root: root, Included: false}}
				if err := config.ReconcileDiscovery(&cfg, old, at.Add(2*time.Minute)); err != nil {
					t.Fatal(err)
				}
				if err := config.Save(home, cfg); err != nil {
					t.Fatal(err)
				}
				now = at.Add(3 * time.Minute)
			}
			if err := ReplayAdmissionIntents(home, now, testDecoders); err != nil {
				t.Fatal(err)
			}
			if got := len(queueFiles(t, home)); got != 0 {
				t.Fatalf("ineligible unavailable-cwd intent remains: %d", got)
			}
			regs, err := state.OpenReadOnly(home).LoadRegistrations()
			if err != nil || len(regs) != 0 {
				t.Fatalf("ineligible intent registered: %#v %v", regs, err)
			}
		})
	}
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

func TestBlanketInterruptedReplayCompletesPhysicalProjectEffects(t *testing.T) {
	for _, kind := range []string{"git-subdirectory", "worktree"} {
		t.Run(kind, func(t *testing.T) {
			home, root, _, at := blanketHookFixture(t)
			cwd := filepath.Join(root, "subdirectory")
			gitdir := filepath.Join(root, ".git", "worktrees", "one")
			if err := os.MkdirAll(gitdir, 0700); err != nil {
				t.Fatal(err)
			}
			if kind == "worktree" {
				cwd = t.TempDir()
			}
			if err := os.MkdirAll(cwd, 0700); err != nil {
				t.Fatal(err)
			}
			if kind == "worktree" {
				for path, body := range map[string]string{filepath.Join(cwd, ".git"): "gitdir: " + gitdir, filepath.Join(gitdir, "commondir"): "../..", filepath.Join(gitdir, "gitdir"): filepath.Join(cwd, ".git")} {
					if err := os.WriteFile(path, []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			observed := at.Add(time.Minute)
			path := filepath.Join(cwd, "fresh.jsonl")
			busy := func(string, time.Duration) (func(), error) { return nil, local.ErrBusy }
			if err := handleEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native", "cwd": cwd, "transcript_path": path}, observed, busy, nil); err != nil {
				t.Fatal(err)
			}
			interrupted := false
			err := replayAdmissionIntents(home, observed.Add(time.Minute), testDecoders, func(effect effectName) error {
				if effect == effectRegistrationCreate {
					interrupted = true
					return errors.New("interrupted after durable registration")
				}
				return nil
			})
			if err == nil || !interrupted {
				t.Fatalf("missing registration interruption: %v", err)
			}
			store := state.OpenReadOnly(home)
			regs, err := store.LoadRegistrations()
			if err != nil || len(regs) != 1 || regs[0].CodexAdmission == nil {
				t.Fatalf("durable registration %#v %v", regs, err)
			}
			before := regs[0]
			if before.ProjectRoot == cwd {
				t.Fatal("fixture did not separate physical root and cwd")
			}
			for range 2 {
				if err := ReplayAdmissionIntents(home, observed.Add(2*time.Minute), testDecoders); err != nil {
					t.Fatal(err)
				}
			}
			regs, err = store.LoadRegistrations()
			if err != nil || len(regs) != 1 || regs[0].ArchiveSessionID != before.ArchiveSessionID || *regs[0].CodexAdmission != *before.CodexAdmission || !regs[0].AdmittedAt.Equal(observed) || !regs[0].SessionStartedAt.Equal(observed) {
				t.Fatalf("replay changed identity %#v %v", regs, err)
			}
			if err := HandleEvent(home, "codex", map[string]any{"hook_event_name": "Stop", "session_id": "native", "cwd": cwd, "transcript_path": path}, observed.Add(3*time.Minute), WithDecoders(testDecoders)); err != nil {
				t.Fatal(err)
			}
			requests, err := store.LoadRequests()
			if err != nil || len(requests) != 1 || !requests[0].Urgent() || len(requests[0].HookEvidence) != 3 {
				t.Fatalf("replay did not complete start/stop effects exactly once %#v %v", requests, err)
			}
			entries, err := os.ReadDir(admissionIntentDir(home))
			if err != nil || len(entries) != 0 {
				t.Fatalf("intent remains after completion %#v %v", entries, err)
			}
		})
	}
}

func TestBlanketHookRetainsWorkingDirectoryCommitObservations(t *testing.T) {
	home, root, _, at := blanketHookFixture(t)
	git := &headLookup{sha: startCommit, dirty: new(true)}
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": root}
	options := []Option{WithDecoders(testDecoders), WithGitHead(git.lookup)}
	if err := HandleEvent(home, "codex", payload, at.Add(time.Minute), options...); err != nil {
		t.Fatal(err)
	}
	reg := onlyRegistration(t, home)
	if reg.CodexAdmission == nil || reg.StartHead == nil || reg.StartHead.SHA != startCommit || reg.StartHead.Dirty == nil || !*reg.StartHead.Dirty {
		t.Fatalf("missing blanket start observation: %#v", reg)
	}
	git.sha = laterCommit
	if err := HandleEvent(home, "codex", stopPayload(root), at.Add(2*time.Minute), options...); err != nil {
		t.Fatal(err)
	}
	reg = onlyRegistration(t, home)
	if reg.LastHead == nil || reg.LastHead.SHA != laterCommit || reg.LastHeadSeenAt == nil || !reg.LastHeadSeenAt.Equal(at.Add(2*time.Minute)) {
		t.Fatalf("missing blanket last observation: %#v", reg)
	}
	if got := git.questions(); len(got) != 2 || got[0] != (headQuestion{root, true}) || got[1] != (headQuestion{root, false}) {
		t.Fatalf("wrong working-directory queries: %#v", got)
	}
}

func TestBlanketCommitObservationsRespectExclusionAndSurviveScopeReduction(t *testing.T) {
	home, root, cfg, at := blanketHookFixture(t)
	git := &headLookup{sha: startCommit}
	options := []Option{WithDecoders(testDecoders), WithGitHead(git.lookup)}
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": root}
	if err := HandleEvent(home, "codex", payload, at.Add(time.Minute), options...); err != nil {
		t.Fatal(err)
	}
	before := onlyRegistration(t, home)
	previous := cfg
	cfg.Archive.Projects = []archive.ProjectActivation{{Root: root, Included: false}}
	if err := config.ReconcileDiscovery(&cfg, previous, at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if err := HandleEvent(home, "codex", stopPayload(root), at.Add(3*time.Minute), options...); err != nil {
		t.Fatal(err)
	}
	after := onlyRegistration(t, home)
	if len(git.questions()) != 1 || after.LastHead != nil || after.LastHeadSeenAt != nil || !after.HookObservedAt.Equal(before.HookObservedAt) {
		t.Fatalf("excluded stop observed git or changed registration: %#v", after)
	}
	previous = cfg
	cfg.Archive.Projects = []archive.ProjectActivation{{Root: filepath.Dir(root), Included: true, ActivatedAt: at.Add(4 * time.Minute)}}
	setTestCodexScope(&cfg, config.CodexIncludedProjects)
	if err := config.ReconcileDiscovery(&cfg, previous, at.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	git.mu.Lock()
	git.sha = laterCommit
	git.mu.Unlock()
	stopAt := at.Add(5 * time.Minute)
	if err := HandleEvent(home, "codex", stopPayload(root), stopAt, options...); err != nil {
		t.Fatal(err)
	}
	after = onlyRegistration(t, home)
	if after.LastHead == nil || after.LastHead.SHA != laterCommit || !after.LastHead.ObservedAt.Equal(stopAt) || after.CodexAdmission.Generation != before.CodexAdmission.Generation || after.StartHead.SHA != before.StartHead.SHA {
		t.Fatalf("scope reduction lost observation or changed immutable start/proof: %#v", after)
	}
	if len(git.questions()) != 2 {
		t.Fatalf("continuation was skipped: %#v", git.questions())
	}
}
