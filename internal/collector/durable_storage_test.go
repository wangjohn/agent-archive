package collector

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type refuseNativeSources struct{ t *testing.T }

func (s refuseNativeSources) LookupSources(string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	s.t.Fatal("refused durable work opened a native provider/filter")
	return nil, nil, false
}

func TestLocalBundleFuturePublishedRefusesBeforeNativeFilter(t *testing.T) {
	s := newTestStore(t)
	if err := os.Remove(filepath.Join(s.Home(), "config.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Home(), "published", "foreign.json"), []byte(`{"commit":null}`), 0600); err != nil {
		t.Fatal(err)
	}
	reg := archive.SessionRegistration{ArchiveSessionID: "foreign", Harness: archive.Harness{Name: "codex"}, TranscriptPath: "/synthetic/never-open"}
	bundle, err := ReadLocalBundle(t.Context(), s.Home(), reg, time.Now(), "", refuseNativeSources{t})
	if !errors.Is(err, state.ErrDurableStorageRecovery) || bundle.ArchiveSessionID != "" {
		t.Fatalf("foreign preview emitted: %+v %v", bundle, err)
	}
}

func TestCollectorSourceOnlyRecoveryWithoutRegistration(t *testing.T) {
	s := newTestStore(t)
	data := []byte("frozen-original")
	sum := sha256.Sum256(data)
	ref := archive.SourceReference{SHA256: hex.EncodeToString(sum[:]), Key: "synthetic", CompressedBytes: len(data)}
	stage, err := s.StagePendingSource("orphan", ref, data)
	if err != nil {
		t.Fatal(err)
	}
	loads := state.PublishedStateLoads()
	result, err := Run(t.Context(), s, storagetest.NewMemoryStore(), Options{MachineID: "synthetic", Sources: refuseNativeSources{t}})
	if err != nil || !errors.Is(result.Errors["orphan"], state.ErrDurableStorageRecovery) || result.Scanned != 0 || len(result.Published) != 0 {
		t.Fatalf("orphan collection: %+v %v", result, err)
	}
	status, err := s.LoadStatus()
	if err != nil || status.PendingCount != 1 {
		t.Fatalf("orphan status: %+v %v", status, err)
	}
	raw, err := s.ReadPendingSource("orphan", stage)
	if err != nil || string(raw) != string(data) {
		t.Fatal("original lost", err)
	}
	if state.PublishedStateLoads() != loads {
		t.Fatal("source-only pass decoded published bodies")
	}
	t.Log("source-only cost: native opens=0, filters=0, published body decodes=0, registration fabricated=0")
}

func TestCollectorAnonymousTempsRefuseBeforeNativeAndRemainCharged(t *testing.T) {
	for _, mode := range []string{"protected", "future", "missing", "corrupt", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			for _, name := range []string{".pending-123", ".pending-abcdef0123456789abcdef0123456789"} {
				t.Run(name, func(t *testing.T) {
					s := newTestStore(t)
					if mode == "protected" {
						if err := config.WithDurableStorage(s.Home(), func(g config.DurableStorageGuard) error { return g.CheckHome(s.Home()) }); err != nil {
							t.Fatal(err)
						}
					}
					switch mode {
					case "future":
						if err := os.WriteFile(filepath.Join(s.Home(), "config.json"), []byte(`{"schema_version":{"version":8,"writer":"publication-composition-v8"},"publication_composition_protection":true}`), 0600); err != nil {
							t.Fatal(err)
						}
					case "missing":
						if err := os.Remove(filepath.Join(s.Home(), "config.json")); err != nil {
							t.Fatal(err)
						}
					case "corrupt":
						if err := os.WriteFile(filepath.Join(s.Home(), "config.json"), []byte("{"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					path := filepath.Join(s.Home(), "pending", name)
					if err := os.WriteFile(path, []byte("sole-original"), 0600); err != nil {
						t.Fatal(err)
					}
					past := time.Now().Add(-2 * time.Hour)
					if err := os.Chtimes(path, past, past); err != nil {
						t.Fatal(err)
					}
					result, err := Run(t.Context(), s, storagetest.NewMemoryStore(), Options{MachineID: "synthetic", Sources: refuseNativeSources{t}})
					if mode == "legacy" {
						if err != nil {
							t.Fatal(err)
						}
						if _, err := os.Stat(path); !os.IsNotExist(err) {
							t.Fatalf("legacy temp not cleaned: %v", err)
						}
						return
					}
					if !errors.Is(err, state.ErrDurableStorageRecovery) || result.Scanned != 0 || len(result.Published) != 0 {
						t.Fatalf("anonymous collection: %+v %v", result, err)
					}
					s.RemoveStaleTemps()
					raw, err := os.ReadFile(path)
					if err != nil || string(raw) != "sole-original" {
						t.Fatal("original discarded", err)
					}
					if mode == "protected" {
						data := []byte("new")
						sum := sha256.Sum256(data)
						_, err = s.StagePendingSource("new", archive.SourceReference{Key: "synthetic", SHA256: hex.EncodeToString(sum[:]), CompressedBytes: len(data)}, data)
						if !errors.Is(err, state.ErrDurableStorageRecovery) {
							t.Fatalf("unknown temp not charged/refused: %v", err)
						}
					}
				})
			}
		})
	}
}

func TestLocalBundleAnonymousTempRefusesBeforeNativeFilter(t *testing.T) {
	s := newTestStore(t)
	if err := os.WriteFile(filepath.Join(s.Home(), "pending", ".pending-123"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := archive.SessionRegistration{ArchiveSessionID: "absent", Harness: archive.Harness{Name: "codex"}, TranscriptPath: "/synthetic/never-open"}
	bundle, err := ReadLocalBundle(t.Context(), s.Home(), reg, time.Now(), "", refuseNativeSources{t})
	if !errors.Is(err, state.ErrDurableStorageRecovery) || bundle.ArchiveSessionID != "" {
		t.Fatalf("anonymous preview emitted: %+v %v", bundle, err)
	}
}
