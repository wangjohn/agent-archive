package cli

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"path/filepath"
	"testing"
	"time"
)

func TestDiscoveryAttemptPrecedesUnavailableStorage(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	env := testEnv(t, home, at)
	setUpTestConfig(t, home, t.TempDir(), at.Add(-time.Hour))
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Harnesses = []string{"codex"}
	cfg.Discovery = &config.DiscoveryConfig{Enabled: true, CodexHomes: []string{t.TempDir()}}
	if err := config.ReconcileDiscovery(&cfg, config.Config{}, at); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return nil, errors.New("synthetic outage") }
	if _, err := runOnePass(env, false); err == nil {
		t.Fatal("outage not reported")
	}
	if discovery.LoadHealth(home).LastAttempt.IsZero() {
		t.Fatal("storage initialization prevented discovery")
	}
}
func TestDiscoveryStatusDoesNotInventHookObservation(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, t.TempDir(), at.Add(-time.Hour))
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	reg := archive.SessionRegistration{ArchiveSessionID: "discovered", NativeSessionID: "synthetic", ProjectID: archive.ProjectID(cfg.Archive.Projects[0].Root), ProjectRoot: cfg.Archive.Projects[0].Root, Harness: archive.Harness{Name: "codex"}, SessionStartedAt: at, AdmittedAt: at, RegisteredAt: at, Origin: archive.SessionOriginDiscovery, TranscriptPath: filepath.Join(t.TempDir(), "missing.jsonl")}
	if err := store.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	view := statusView{}
	sessions := readSessionStatus(&view, cfg, home, store)
	app := appStatus{}
	pair := projectCaptureStatus{}
	var outcome verificationOutcome
	sessions.addSession(&app, &pair, reg, cfg, home, nil, &outcome)
	if app.HookObserved || pair.HookObserved {
		t.Fatal("discovery invented hook observation")
	}
	reg.HookObservedAt = at.Add(time.Minute)
	sessions.addSession(&app, &pair, reg, cfg, home, nil, &outcome)
	if !app.HookObserved || !pair.HookObserved {
		t.Fatal("actual hook observation lost")
	}
}
