package cli

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"os"
	"path/filepath"
	"strings"
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
	var catalog struct {
		Health discovery.Health `json:"health"`
	}
	if err := local.Read(filepath.Join(home, "discovery-catalog.json"), &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.Health.LastAttempt.IsZero() {
		t.Fatal("storage initialization prevented discovery")
	}
}

func TestDiscoveryFailureSurvivesSuccessfulCollector(t *testing.T) {
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
	// A directory at the catalog path deterministically prevents atomic writes,
	// including when tests run with root privileges.
	if err := os.Mkdir(filepath.Join(home, "discovery-catalog.json"), 0700); err != nil {
		t.Fatal(err)
	}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return storagetest.NewMemoryStore(), nil }
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	status, err := state.OpenReadOnly(home).LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(status.LastErrors, "\n"), "discovery state write failed") {
		t.Fatalf("discovery failure lost: %#v", status.LastErrors)
	}
}
