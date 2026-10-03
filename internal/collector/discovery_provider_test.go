package collector

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestDiscoveryProviderRetainsConfinementAndAdmissionIdentity(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"intact", "changed-id", "changed-start", "changed-cwd", "symlink", "component-symlink"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			root, project := t.TempDir(), t.TempDir()
			at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			native := "00000000-0000-0000-0000-000000000001"
			path := filepath.Join(root, "rollout-"+native+".jsonl")
			payload := map[string]any{"id": native, "timestamp": at.Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "synthetic", "cli_version": "test"}
			switch scenario {
			case "changed-id":
				payload["id"] = "00000000-0000-0000-0000-000000000002"
			case "changed-start":
				payload["timestamp"] = at.Add(time.Minute).Format(time.RFC3339Nano)
			case "changed-cwd":
				payload["cwd"] = t.TempDir()
			}
			raw, err := json.Marshal(map[string]any{"type": "session_meta", "timestamp": payload["timestamp"], "payload": payload})
			if err != nil {
				t.Fatal(err)
			}
			target := path
			if scenario == "component-symlink" {
				realDir := filepath.Join(root, "real")
				if err := os.Mkdir(realDir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(realDir, filepath.Join(root, "alias")); err != nil {
					t.Fatal(err)
				}
				target = filepath.Join(realDir, "rollout-"+native+".jsonl")
				path = filepath.Join(root, "alias", "rollout-"+native+".jsonl")
			}
			if scenario == "symlink" {
				target = filepath.Join(t.TempDir(), "outside.jsonl")
			}
			task, err := json.Marshal(map[string]any{"type": "event_msg", "timestamp": payload["timestamp"], "payload": map[string]any{"type": "task_started", "turn_id": payload["id"], "root_turn_id": payload["id"], "started_at": payload["timestamp"]}})
			if err != nil {
				t.Fatal(err)
			}
			content := append(append(append(raw, '\n'), task...), '\n')
			if err := os.WriteFile(target, content, 0600); err != nil {
				t.Fatal(err)
			}
			if scenario == "symlink" {
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			reg := archive.SessionRegistration{NativeSessionID: native, Origin: archive.SessionOriginDiscovery, SourceKind: archive.SourceKindFile, TranscriptPath: path, DiscoveryRoot: root, DiscoveryCwd: project, SessionStartedAt: at, Harness: archive.Harness{Name: "codex", Version: "test"}, DiscoveryProducerOriginator: "synthetic", DiscoveryProducerSource: "cli"}
			opts := Options{Sources: testSources}
			closePass := openCursorPass(nil, &opts)
			defer func() {
				if err := closePass(); err != nil {
					t.Error(err)
				}
			}()
			// Seed an unrestricted hook pass for this provider. Discovery must open
			// its own confined pass instead of inheriting those read permissions.
			hook := reg
			hook.Origin = archive.SessionOriginHook
			source, _ := newSourceReader(hook, opts)
			_, _ = source.Signature(context.Background())
			source, ok := newSourceReader(reg, opts)
			if !ok {
				t.Fatal("discovery reader missing")
			}
			_, signatureErr := source.Signature(context.Background())
			if (scenario == "symlink" || scenario == "component-symlink") && signatureErr == nil {
				t.Fatal("signature skipped strict snapshot validation")
			}
			adapter, err := testAdapter("codex")
			if err != nil {
				t.Fatal(err)
			}
			_, observed, err := source.Filter(context.Background(), adapter, DefaultMaxTranscriptBytes)
			if scenario == "intact" {
				if err != nil || !observed.observation.Present || observed.observation.Signature.Provider == "" {
					t.Fatalf("intact source lost provider observation: %+v %v", observed, err)
				}
			} else if err == nil {
				t.Fatal("changed or unconfined source accepted")
			}
		})
	}
}
