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
