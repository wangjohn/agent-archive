package cli

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/local"
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
