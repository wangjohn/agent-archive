package cli

import (
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
	for round := 0; round < rounds; round++ {
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
