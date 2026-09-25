package cli

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestSetupFailureBeforeCommitRecoversFreshInstall(t *testing.T) {
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	env.LoadLaunchAgent = func(string) error { return errors.New("bootstrap failed") }
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, project), 1)
	if _, found, _ := config.Load(home); found {
		t.Fatal("failed fresh setup left active config")
	}
	if _, err := os.Stat(filepath.Join(userHome, ".codex", "hooks.json")); !os.IsNotExist(err) {
		t.Fatal("failed setup left hooks")
	}
	if transactionPending(home) {
		t.Fatal("journal not cleaned after restoration")
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestResumeDraftLeftAfterDestinationCommitPreservesOwnership(t *testing.T) {
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	now := time.Now().UTC()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)
	setupRun(t, env, s3SetupInput("original", "us-east-1", "profile", true, false, false, project), 0)
	old, _, _ := config.Load(home)
	changed := old
	changed.Storage.Bucket = "new-bucket"
	stale := setupDraft{Version: 1, Step: 2, Config: changed}
	// Persist the pre-commit draft, then commit without the wizard's deletion:
	// exactly the state left by a crash or failed draft removal after apply.
	if err := local.Write(filepath.Join(home, "setup-draft.json"), stale); err != nil {
		t.Fatal(err)
	}
	next := stale.Config
	exe, err := env.executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := applySetup(home, userHome, exe, old, &next, nil, env); err != nil {
		t.Fatal(err)
	}
	committed, _, _ := config.Load(home)
	if committed.DestinationSince.IsZero() {
		t.Fatal("switch did not establish boundary")
	}
	env.Now = func() time.Time { return now.Add(time.Hour) }
	setupRun(t, env, "continue\ny\n", 0)
	resumed, _, _ := config.Load(home)
	if !resumed.DestinationSince.Equal(committed.DestinationSince) || !reflect.DeepEqual(resumed.PreviousDestinations, committed.PreviousDestinations) {
		t.Fatalf("ownership changed: before=%+v after=%+v", committed, resumed)
	}
	if resumed.AcceptSession(archive.SessionRegistration{SessionStartedAt: now.Add(-time.Minute), Harness: archive.Harness{Name: "codex"}, ProjectRoot: resumed.Archive.Projects[0].Root}) {
		t.Fatal("retired session became eligible")
	}
}
