package capture

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

// BenchmarkHookLocalEffects measures local work without process startup or a
// native application. All locations and repository observations are synthetic.
func BenchmarkHookLocalEffects(b *testing.B) {
	for _, scenario := range []string{"fresh", "stop", "subagent", "disabled", "paused", "ignored"} {
		b.Run(scenario, func(b *testing.B) {
			home, project := b.TempDir(), b.TempDir()
			now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
			cfg := config.Config{MachineID: "synthetic", Storage: credentialsTestConfig(), Archive: archive.Config{
				SchemaVersion: 1, MachineID: "synthetic", Enabled: scenario != "disabled", Projects: []archive.ProjectActivation{{ProjectID: "synthetic", Root: project, Included: true, ActivatedAt: now.Add(-time.Hour)}},
			}, Paused: scenario == "paused"}
			if err := config.Save(home, cfg); err != nil {
				b.Fatal(err)
			}
			harness := "codex"
			if scenario == "subagent" {
				harness = "claude"
			}
			repoCalls := 0
			option := WithRepoKey(func(string) string { repoCalls++; return "" })
			if err := HandleEvent(home, harness, map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "synthetic", "cwd": project}, now, option, WithDecoders(testDecoders)); err != nil {
				b.Fatal(err)
			}
			child := filepath.Join(b.TempDir(), "child.jsonl")
			if err := os.WriteFile(child, []byte("synthetic"), 0600); err != nil {
				b.Fatal(err)
			}
			payload := map[string]any{"hook_event_name": "Stop", "session_id": "synthetic"}
			if scenario == "subagent" {
				payload = map[string]any{"hook_event_name": "SubagentStop", "session_id": "synthetic", "agent_id": "child", "agent_transcript_path": child}
			}
			if scenario == "ignored" {
				payload["hook_event_name"] = "PreToolUse"
			}
			repoCalls = 0
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				if scenario == "fresh" {
					payload = map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": fmt.Sprintf("fresh-%d", i), "cwd": project}
				}
				if err := HandleEvent(home, harness, payload, now, option, WithDecoders(testDecoders)); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if scenario != "fresh" && repoCalls != 0 {
				b.Fatalf("%s probed repository %d times", scenario, repoCalls)
			}
			if scenario == "fresh" && repoCalls != b.N {
				b.Fatalf("fresh calls=%d, want %d", repoCalls, b.N)
			}
			if scenario == "subagent" {
				local, err := state.Open(home)
				if err != nil {
					b.Fatal(err)
				}
				candidates, err := local.LoadSubagentCandidates()
				if err != nil || len(candidates) != 1 {
					b.Fatalf("subagent effect: %d candidates, %v", len(candidates), err)
				}
			}
			b.ReportMetric(float64(repoCalls)/float64(b.N), "repo-probes/op")
		})
	}
}
