package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestAllCodexStatusImportsDoNotRequireFreshProjectCapture(t *testing.T) {
	t.Parallel()
	for _, all := range []bool{false, true} {
		t.Run(map[bool]string{false: "included", true: "all"}[all], func(t *testing.T) {
			t.Parallel()
			home, userHome, a, b := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
			if err := os.Chmod(home, 0o700); err != nil {
				t.Fatal(err)
			}
			a, err := filepath.EvalSymlinks(a)
			must(t, err)
			b, err = filepath.EvalSymlinks(b)
			must(t, err)
			now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
			cfg := pairTestConfig(now, []string{"codex"}, a, b)
			if all {
				must(t, config.SetCodexCaptureScope(&cfg, config.CodexAllProjects))
				must(t, config.ReconcileDiscovery(&cfg, config.Config{}, now))
			}
			must(t, config.Save(home, cfg))
			store, err := state.Open(home)
			must(t, err)
			remote := storagetest.NewMemoryStore()
			publishPairSession(t, home, store, remote, cfg, now, "fresh-a", a, true)
			env := pairStatusEnv(t, home, userHome, now, "codex")
			baseline, err := readStatus(env)
			must(t, err)
			if baseline.Apps[0].ReadBackVerified != all {
				t.Fatal("baseline capture checklist changed")
			}
			imported := saveImportedSession(t, store, now, "historical-b", b)
			result, err := collector.Run(context.Background(), store, remote, collector.Options{Sources: productionAgents, MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return now }})
			must(t, err)
			if len(result.Errors) > 0 {
				t.Fatalf("import publication: %+v", result.Errors)
			}
			summary, err := verifyPublications(home, cfg, testEnv(t, home, now), store, remote)
			must(t, err)
			if summary.Verified != 1 {
				t.Fatalf("import was not actually read back: %+v", summary)
			}
			check := func(observed bool) {
				t.Helper()
				view, err := readStatus(env)
				must(t, err)
				app := view.Apps[0]
				if app.Sessions != 1 || app.PublishedSessions != 1 || app.VerifiedSessions != 1 || app.ImportedSessions != 1 || (!observed && app.UploadingSessions != 0) || app.ReadBackVerified != all {
					t.Fatalf("wrong automatic/import evidence: %+v", app)
				}
				if len(app.Projects) != 2 {
					t.Fatal("import-only project row disappeared")
				}
				for _, pair := range app.Projects {
					if pair.ProjectRoot == b && (pair.HookObserved != observed || pair.ReadBackVerified || pair.imported != 1) {
						t.Fatalf("import row evidence changed: %+v", pair)
					}
				}
				if all && (strings.Contains(view.Next, b) || readBackProgress(app) != "1 of 1 projects verified") {
					t.Fatalf("import-only root became automatic checklist: %s; %s", view.Next, readBackProgress(app))
				}
				if !all && !strings.Contains(view.Next, b) {
					t.Fatal("included-project checklist lost B")
				}
			}
			check(false)
			must(t, handleTestHookEvent(home, "codex", map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": imported.NativeSessionID, "cwd": b}, now.Add(time.Minute)))
			result, err = collector.Run(context.Background(), store, remote, collector.Options{Sources: productionAgents, MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return now.Add(time.Minute) }})
			must(t, err)
			if len(result.Errors) > 0 {
				t.Fatalf("observed import publication: %+v", result.Errors)
			}
			_, err = verifyPublications(home, cfg, testEnv(t, home, now.Add(time.Minute)), store, remote)
			must(t, err)
			check(true)
		})
	}
}

func TestAllCodexStatusWithOnlyImportsWaitsForAnyEligibleTask(t *testing.T) {
	t.Parallel()
	home, userHome, b := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	cfg := pairTestConfig(now, []string{"codex"}, b)
	must(t, config.SetCodexCaptureScope(&cfg, config.CodexAllProjects))
	must(t, config.ReconcileDiscovery(&cfg, config.Config{}, now))
	must(t, config.Save(home, cfg))
	store, err := state.Open(home)
	must(t, err)
	saveImportedSession(t, store, now, "historical-only", b)
	view, err := readStatus(pairStatusEnv(t, home, userHome, now, "codex"))
	must(t, err)
	if view.Apps[0].ReadBackVerified || view.Apps[0].ImportedSessions != 1 || strings.Contains(view.Next, b) || !strings.Contains(view.Next, "any non-excluded project") {
		t.Fatalf("historical import established or constrained automatic capture: %+v", view)
	}
}
