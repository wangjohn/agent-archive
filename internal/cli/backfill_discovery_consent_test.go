package cli

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
)

func TestBackfillScopeChangesReconcileExistingDiscoveryConsent(t *testing.T) {
	t.Parallel()
	f, _ := newImportFixture(t)
	cfg := mustLoadConfig(t, f.data)
	cfg.Harnesses = append(cfg.Harnesses, "codex")
	cfg.Discovery = &config.DiscoveryConfig{Enabled: true, CodexHomes: []string{filepath.Join(f.userHome, ".codex")}}
	must(t, config.ReconcileDiscovery(&cfg, config.Config{}, backfillNow.Add(-time.Hour)))
	must(t, config.Save(f.data, cfg))
	original := cfg.Discovery.Authorizations[0]
	if out, errOut, code := f.importRun(t, nil, false, "--yes"); code != 0 {
		t.Fatalf("import: %d %s %s", code, errOut, out)
	}
	imported := mustLoadConfig(t, f.data)
	if len(imported.Discovery.Authorizations) != len(imported.Archive.Projects) {
		t.Fatalf("import left project authorization stale: %+v", imported.Discovery.Authorizations)
	}
	for _, auth := range imported.Discovery.Authorizations {
		if auth.ProjectRoot == original.ProjectRoot {
			if auth.Generation != original.Generation {
				t.Fatal("unrelated import replaced existing project consent")
			}
		} else if _, allowed := imported.DiscoveryGeneration("codex", auth.ProjectRoot, backfillNow.Add(-time.Nanosecond), backfillNow); allowed {
			t.Fatal("new import project admitted a task predating its authorization")
		}
	}
	if _, errOut, code := f.undoRun(t, nil, false, "--yes"); code != 0 {
		t.Fatalf("undo: %d %s", code, errOut)
	}
	undone := mustLoadConfig(t, f.data)
	if len(undone.Discovery.Authorizations) != 1 || undone.Discovery.Authorizations[0].Generation != original.Generation {
		t.Fatalf("undo retained removed project consent: %+v", undone.Discovery.Authorizations)
	}
}
