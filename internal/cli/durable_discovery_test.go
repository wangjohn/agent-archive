package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type discoveryRecoveryKind string

const (
	discoveryHealthy        discoveryRecoveryKind = "healthy"
	discoveryPendingTemp    discoveryRecoveryKind = "pending-temp"
	discoveryGenerationTemp discoveryRecoveryKind = "generation-temp"
	discoveryOpaqueRegular  discoveryRecoveryKind = "opaque-regular"
	discoveryHistorySymlink discoveryRecoveryKind = "history-symlink"
)

func TestCollectAnonymousRecoveryPrecedesDiscovery(t *testing.T) {
	for _, kind := range []discoveryRecoveryKind{discoveryHealthy, discoveryPendingTemp, discoveryGenerationTemp, discoveryOpaqueRegular, discoveryHistorySymlink} {
		t.Run(string(kind), func(t *testing.T) {
			home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
			must(t, os.Chmod(home, 0700))
			now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			nativeHome := filepath.Join(userHome, ".codex")
			sessions := filepath.Join(nativeHome, "sessions")
			must(t, os.MkdirAll(sessions, 0700))
			raw := `{"type":"session_meta","payload":{"id":"11111111-1111-4111-8111-111111111111","cwd":"` + project + `","timestamp":"2026-10-01T12:00:00Z","cli_version":"0.160.0","originator":"codex_cli_rs","source":"cli"}}` + "\n"
			must(t, os.WriteFile(filepath.Join(sessions, "rollout-2026-10-01T12-00-00-11111111-1111-4111-8111-111111111111.jsonl"), []byte(raw), 0600))
			cfg := config.Config{MachineID: "machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, Included: true, ActivatedAt: now.Add(-time.Hour)}}}, Discovery: &config.DiscoveryConfig{ChoiceRecorded: true, Enabled: true, CodexHomes: []string{nativeHome}}}
			must(t, config.Save(home, cfg))
			local, err := state.Open(home)
			must(t, err)
			owed := kind != discoveryHealthy
			var owedPath string
			original := []byte("sole-original")
			if owed {
				must(t, config.WithDurableStorage(home, func(config.DurableStorageGuard) error { return nil }))
				switch kind {
				case discoveryHealthy:
				case discoveryPendingTemp:
					owedPath = filepath.Join(home, "pending", ".pending-123")
				case discoveryGenerationTemp:
					owedPath = filepath.Join(home, "generation-recovery", ".pending-123")
				case discoveryOpaqueRegular:
					owedPath = filepath.Join(home, "publication-evidence", "opaque")
				case discoveryHistorySymlink:
					owedPath = filepath.Join(home, "sessions", "unknown-history")
				}
				must(t, os.MkdirAll(filepath.Dir(owedPath), 0700))
				if kind == discoveryHistorySymlink {
					must(t, os.Symlink(t.TempDir(), owedPath))
				} else {
					must(t, os.WriteFile(owedPath, original, 0600))
				}
			}
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return storagetest.NewMemoryStore(), nil }
			_, err = runOnePass(env, true)
			health, found, healthErr := discovery.ReadHealth(home)
			if !owed {
				if err != nil || healthErr != nil || !found || health.Probes == 0 {
					t.Fatalf("healthy discovery absent: health=%+v found=%t err=%v/%v", health, found, err, healthErr)
				}
				return
			}
			if !errors.Is(err, state.ErrDurableStorageRecovery) {
				t.Errorf("anonymous recovery not refused: %v", err)
			}
			if found || health.Probes != 0 || health.NativeReadOperations != 0 {
				t.Errorf("anonymous recovery reached Inspect: health=%+v found=%t", health, found)
			}
			if _, err := os.Stat(filepath.Join(home, "discovery-catalog.json")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("discovery checkpoint changed: %v", err)
			}
			obligations, censusErr := local.DurableStorageObligations()
			if len(obligations) == 0 || censusErr != nil && !errors.Is(censusErr, state.ErrDurableStorageRecovery) {
				t.Error("owed bytes/obligation disappeared", censusErr)
			}
			if kind == discoveryHistorySymlink {
				if info, err := os.Lstat(owedPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatal("unknown history changed", err)
				}
			} else if raw, err := os.ReadFile(owedPath); err != nil || string(raw) != string(original) {
				t.Fatal("original obligation changed", err)
			}
		})
	}
}

func TestDurableDiscoveryPreflightUsesPassCancellation(t *testing.T) {
	home := t.TempDir()
	must(t, os.Chmod(home, 0700))
	must(t, config.Save(home, config.Config{MachineID: "synthetic"}))
	store, err := state.Open(home)
	must(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := checkDurablePassRoots(ctx, store); !errors.Is(err, context.Canceled) {
		t.Fatal("preflight ignored pass cancellation", err)
	}
	if err := checkDurablePassRoots(t.Context(), store); err != nil {
		t.Fatal("canceled observation cached absence/refusal", err)
	}
}
