package cli

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// twoProjectConfig saves a configuration with both roots, including only
// those listed in included, and returns its projects.
func twoProjectConfig(t *testing.T, home string, included ...string) []archive.ProjectActivation {
	t.Helper()
	activated := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var projects []archive.ProjectActivation
	for _, root := range []string{"/work/kept", "/work/excluded"} {
		in := false
		for _, r := range included {
			in = in || r == root
		}
		projects = append(projects, archive.ProjectActivation{ProjectID: archive.ProjectID(root), Root: root, Included: in, ActivatedAt: activated})
	}
	cfg := config.Config{
		MachineID: "machine-1", Storage: credentialsTestConfig(),
		Archive: archive.Config{SchemaVersion: 1, MachineID: "machine-1", Enabled: true, Projects: projects},
	}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	return projects
}

func diagnosticFor(root string, at time.Time) captureDiagnostic {
	return captureDiagnostic{Code: diagnosticSetupInProgress, Harness: "claude", ProjectRoot: root, ObservedAt: at}
}

func storedRoots(t *testing.T, home string) map[string]bool {
	t.Helper()
	diagnostics, err := readCaptureDiagnostics(home)
	if err != nil {
		t.Fatal(err)
	}
	roots := map[string]bool{}
	for _, d := range diagnostics {
		roots[d.ProjectRoot] = true
	}
	return roots
}

// The resurrection case, made deterministic: a hook decided the project was
// included from the configuration it read during setup's transaction; setup
// then committed the exclusion and pruned; only then does the hook write.
func TestPrunedProjectDiagnosticIsNotResurrectedByAStaleHook(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	twoProjectConfig(t, home, "/work/kept", "/work/excluded")
	if err := recordCaptureDiagnostic(home, diagnosticFor("/work/excluded", at)); err != nil {
		t.Fatal(err)
	}

	projects := twoProjectConfig(t, home, "/work/kept") // setup commits the exclusion
	if err := pruneCaptureDiagnostics(home, projects); err != nil {
		t.Fatal(err)
	}
	if err := recordCaptureDiagnostic(home, diagnosticFor("/work/excluded", at.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	if err := recordCaptureDiagnostic(home, diagnosticFor("/work/kept", at.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	roots := storedRoots(t, home)
	if roots["/work/excluded"] || !roots["/work/kept"] {
		t.Fatalf("stored roots after prune = %v", roots)
	}
}

// A hook never waits more than hookDiagnosticsWait for the lock; on timeout it
// drops the diagnostic and returns without error.
func TestHookDiagnosticWaitIsBoundedAndDropsOnTimeout(t *testing.T) {
	home := t.TempDir()
	twoProjectConfig(t, home, "/work/kept")
	release, err := local.NamedLock(home, diagnosticsLockName)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = recordCaptureDiagnostic(home, diagnosticFor("/work/kept", start))
	elapsed := time.Since(start)
	release()
	if err != nil {
		t.Fatalf("a busy lock must drop the diagnostic, not fail the hook: %v", err)
	}
	if elapsed < hookDiagnosticsWait || elapsed > hookDiagnosticsWait+200*time.Millisecond {
		t.Fatalf("hook waited %s, want about %s", elapsed, hookDiagnosticsWait)
	}
	if len(storedRoots(t, home)) != 0 {
		t.Fatal("a diagnostic was written without the lock")
	}
	// Uncontended, it records.
	if err := recordCaptureDiagnostic(home, diagnosticFor("/work/kept", start)); err != nil || !storedRoots(t, home)["/work/kept"] {
		t.Fatalf("uncontended record failed: %v", err)
	}
}

// Setup's prune waits for a hook that holds the lock instead of racing it.
func TestPruneWaitsForAHookHoldingTheLock(t *testing.T) {
	home := t.TempDir()
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	twoProjectConfig(t, home, "/work/kept", "/work/excluded")
	if err := recordCaptureDiagnostic(home, diagnosticFor("/work/excluded", at)); err != nil {
		t.Fatal(err)
	}
	projects := twoProjectConfig(t, home, "/work/kept")
	release, err := local.NamedLock(home, diagnosticsLockName)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		release()
	}()
	if err := pruneCaptureDiagnostics(home, projects); err != nil {
		t.Fatalf("prune gave up instead of waiting: %v", err)
	}
	if storedRoots(t, home)["/work/excluded"] {
		t.Fatal("prune did not remove the excluded project's diagnostic")
	}
}

// The race itself: setup commits an exclusion and prunes while a hook, which
// read the old configuration, records a diagnostic for the excluded project.
// Whatever the interleaving, the excluded project's diagnostic is gone at
// the end, and an included project's diagnostic is never lost to the prune.
//
// The hook's wait is raised for this test only. At 50 ms a slow fsync on a
// busy machine makes a hook drop its diagnostic by design, which would be
// indistinguishable here from a lost update; the bounded wait has its own test.
func TestSetupPruneAndHookDiagnosticRaceNeverResurrects(t *testing.T) {
	const rounds = 40
	saved := hookDiagnosticsWait
	hookDiagnosticsWait = 10 * time.Second
	t.Cleanup(func() { hookDiagnosticsWait = saved })
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for round := range rounds {
		home := t.TempDir()
		twoProjectConfig(t, home, "/work/kept", "/work/excluded")
		if err := recordCaptureDiagnostic(home, diagnosticFor("/work/excluded", at)); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var pruneErr, excludedErr, keptErr error
		wg.Add(3)
		go func() { // setup: commit, then prune
			defer wg.Done()
			pruneErr = pruneCaptureDiagnostics(home, twoProjectConfig(t, home, "/work/kept"))
		}()
		go func() { // a hook holding the pre-commit view of the excluded project
			defer wg.Done()
			time.Sleep(time.Duration(round) * 25 * time.Microsecond)
			excludedErr = recordCaptureDiagnostic(home, diagnosticFor("/work/excluded", at.Add(time.Minute)))
		}()
		go func() { // a hook for a project that stays included
			defer wg.Done()
			time.Sleep(time.Duration(rounds-round) * 25 * time.Microsecond)
			keptErr = recordCaptureDiagnostic(home, diagnosticFor("/work/kept", at.Add(time.Minute)))
		}()
		wg.Wait()
		if pruneErr != nil || excludedErr != nil || keptErr != nil {
			t.Fatalf("round %d: prune=%v excluded=%v kept=%v", round, pruneErr, excludedErr, keptErr)
		}
		roots := storedRoots(t, home)
		if roots["/work/excluded"] {
			t.Fatalf("round %d: an excluded project's diagnostic was resurrected", round)
		}
		if !roots["/work/kept"] {
			t.Fatalf("round %d: an included project's diagnostic was lost", round)
		}
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestExcludedProjectLeavesNoDiagnostic(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Now().UTC()
	setUpTestConfig(t, home, "/work/widget", at.Add(-time.Hour))
	if err := handleHookEvent(home, "cursor", map[string]any{"hook_event_name": "sessionStart", "conversation_id": "old", "workspace_roots": []any{"/private/excluded"}}, at); err != nil {
		t.Fatal(err)
	}
	ds, _ := readCaptureDiagnostics(home)
	if len(ds) != 0 {
		t.Fatalf("excluded paths recorded %+v", ds)
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestResumeBeforeActivationRecordsActivationDiagnostic(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Now().UTC()
	setUpTestConfig(t, home, "/work/widget", at.Add(time.Hour))
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "resume", "session_id": "old", "cwd": "/work/widget"}
	if err := handleHookEvent(home, "claude", payload, at); err != nil {
		t.Fatal(err)
	}
	ds, _ := readCaptureDiagnostics(home)
	if len(ds) != 1 || ds[0].Code != diagnosticPreActivationStart {
		t.Fatalf("diagnostics %+v", ds)
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestExcludedProjectDiagnosticLeavesStatus(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Now().UTC()
	env := testEnv(t, home, at)
	setUpTestConfig(t, home, "/work/widget", at.Add(-time.Hour))
	resumed := writeTestTranscript(t, "old.jsonl", "{\"role\":\"user\"}\n")
	if err := handleHookEvent(home, "cursor", map[string]any{"hook_event_name": "sessionStart", "conversation_id": "old", "workspace_roots": []any{"/work/widget"}, "transcript_path": resumed}, at); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if code := runStatusCommand(nil, &out, os.Stderr, env); code != 0 || !strings.Contains(out.String(), "Capture skipped in /work/widget") {
		t.Fatalf("status exit=%d output=%s", code, out.String())
	}
	// The project is excluded afterwards: its path must stop appearing.
	setUpTestConfig(t, home, "/work/kept", at.Add(-time.Hour))
	out.Reset()
	if code := runStatusCommand(nil, &out, os.Stderr, env); code != 0 || strings.Contains(out.String(), "/work/widget") {
		t.Fatalf("status exit=%d output=%s", code, out.String())
	}
	out.Reset()
	if code := runStatusCommand([]string{"--json"}, &out, os.Stderr, env); code != 0 || strings.Contains(out.String(), "/work/widget") || strings.Contains(out.String(), "capture_diagnostics") {
		t.Fatalf("status --json exit=%d output=%s", code, out.String())
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestSetupExcludingProjectPrunesStoredDiagnostic(t *testing.T) {
	t.Parallel()
	home, first, second := t.TempDir(), t.TempDir(), t.TempDir()
	now := time.Now().UTC()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), now)
	setupRun(t, env, s3SetupInput("bucket", "us-east-1", "profile", true, false, false, first), 0)
	cfg, _, _ := config.Load(home)
	root := cfg.Archive.Projects[0].Root
	if err := handleHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "resume", "session_id": "old", "cwd": root}, now); err != nil {
		t.Fatal(err)
	}
	if ds, _ := readCaptureDiagnostics(home); len(ds) != 1 || ds[0].ProjectRoot != root {
		t.Fatalf("diagnostics %+v", ds)
	}
	// Reconfigure capture: drop the first project and include the second.
	setupRun(t, env, "capture\nn\nn\nn\nn\n"+second+"\n\ny\n", 0)
	cfg, _, _ = config.Load(home)
	if len(cfg.Archive.Projects) != 1 || cfg.Archive.Projects[0].Root == root {
		t.Fatalf("projects %+v", cfg.Archive.Projects)
	}
	if ds, err := readCaptureDiagnostics(home); err != nil || len(ds) != 0 {
		t.Fatalf("excluded project diagnostic kept on disk: %+v err=%v", ds, err)
	}
}
