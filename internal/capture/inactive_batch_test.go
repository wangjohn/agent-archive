package capture

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

func TestInactiveBatchAvoidsEvidenceCopyAndFiltering(t *testing.T) {
	for _, paused := range []bool{false, true} {
		home, project := t.TempDir(), t.TempDir()
		at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		setUpTestConfig(t, home, project, at.Add(-time.Hour))
		cfg, _, err := config.Load(home)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Archive.Enabled, cfg.Paused = paused, paused
		if err := config.Save(home, cfg); err != nil {
			t.Fatal(err)
		}
		batch := syntheticLifecycle(agentapi.HookInput{Payload: syntheticPayload(signalBegin, "native", project, ""), ObservedAt: at})
		plain := append([]agentapi.LifecycleEvent(nil), batch...)
		plain[1].Evidence = nil
		batch[1].Evidence = make([]archive.SupplementalEvidence, 16)
		for i := range batch[1].Evidence {
			batch[1].Evidence[i] = archive.SupplementalEvidence{Kind: archive.EvidenceKindLifecycleHook, ObservedAt: at, Provenance: "hook:synthetic:begin", Payload: map[string]any{"event_name": "begin", "model": "synthetic", "text": strings.Repeat("private input ", 100)}}
		}
		measure := func(events []agentapi.LifecycleEvent) float64 {
			return testing.AllocsPerRun(20, func() {
				if err := HandleBatch(home, "synthetic", events, at); err != nil {
					t.Fatal(err)
				}
			})
		}
		plainAllocs, richAllocs := measure(plain), measure(batch)
		t.Logf("paused=%t plain=%.0f rich=%.0f allocations", paused, plainAllocs, richAllocs)
		// Leave room for runtime/race bookkeeping; filtering these sixteen
		// candidates previously added more than four hundred allocations.
		if richAllocs > plainAllocs+8 {
			t.Fatalf("inactive evidence caused allocations: plain %.0f rich %.0f", plainAllocs, richAllocs)
		}
		if batch[1].Evidence[0].Payload["text"] == nil {
			t.Fatal("caller evidence mutated")
		}
	}
}

type malformedBatchCase string

const (
	malformedProvenance malformedBatchCase = "provenance"
	malformedIdentity   malformedBatchCase = "identity"
	malformedCount      malformedBatchCase = "count"
)

func TestInactiveAndPendingSetupRejectWholeMalformedBatch(t *testing.T) {
	for _, pending := range []bool{false, true} {
		for _, invalid := range []malformedBatchCase{malformedProvenance, malformedIdentity, malformedCount} {
			home, project := t.TempDir(), t.TempDir()
			at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			setUpTestConfig(t, home, project, at.Add(-time.Hour))
			cfg, _, err := config.Load(home)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Paused = true
			if err := config.Save(home, cfg); err != nil {
				t.Fatal(err)
			}
			if pending {
				if err := os.WriteFile(setupjournal.JournalPath(home), []byte(`{"changes":[],"plist":""}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			batch := syntheticLifecycle(agentapi.HookInput{Payload: syntheticPayload(signalBegin, "native", project, ""), ObservedAt: at})
			switch invalid {
			case malformedProvenance:
				batch[1].Evidence[0].Provenance = " \t "
			case malformedIdentity:
				batch[1].Session.NativeID = "other"
			case malformedCount:
				batch = append(batch, make([]agentapi.LifecycleEvent, 64)...)
			}
			before, err := os.ReadDir(home)
			if err != nil {
				t.Fatal(err)
			}
			if err := HandleBatch(home, "synthetic", batch, at); err == nil {
				t.Fatalf("accepted %s pending=%t", invalid, pending)
			}
			after, err := os.ReadDir(home)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("malformed batch mutated disk: %v", err)
			}
		}
	}
}

func TestEmptyBatchDoesNotReadConfiguration(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte("invalid configuration"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := HandleBatch(home, "synthetic", nil, time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func TestPendingSetupBatchLeavesOnlyContentFreeDiagnostic(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	if err := os.WriteFile(setupjournal.JournalPath(home), []byte(`{"changes":[],"plist":""}`), 0600); err != nil {
		t.Fatal(err)
	}
	batch := syntheticLifecycle(agentapi.HookInput{Payload: syntheticPayload(signalBegin, "native", project, ""), ObservedAt: at})
	const private = "private-input-never-retained-by-setup"
	batch[1].Evidence[0].Payload["text"] = private
	if err := HandleBatch(home, "synthetic", batch, at); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := ReadDiagnostics(home)
	if err != nil || len(diagnostics) != 1 || diagnostics[0].Code != DiagnosticSetupInProgress {
		t.Fatalf("diagnostics %+v: %v", diagnostics, err)
	}
	if err := filepath.WalkDir(home, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), private) {
			t.Errorf("private input persisted at %s", path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if batch[1].Evidence[0].Payload["text"] != private {
		t.Fatal("caller evidence mutated")
	}
}
