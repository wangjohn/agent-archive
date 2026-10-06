package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/state"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Initial admission benchmark includes the real native registry, source snapshot,
// current permission checks and durable registration. Publication is measured by
// the separate combined-stack end-to-end suite, never inferred from these costs.
func BenchmarkNativeChildInitialAdmission(b *testing.B) {
	for _, count := range []int{1000, 10000, 100000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			for iteration := 0; iteration < b.N; iteration++ {
				b.StopTimer()
				store, cfg, at, home := fixture(b)
				id := fmt.Sprintf("00000000-0000-0000-0000-%012d", iteration+100)
				parent := "00000000-0000-0000-0000-000000000001"
				meta := map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "session_id": parent, "parent_thread_id": parent, "cwd": cfg.Archive.Projects[0].Root, "timestamp": "2026-10-01T12:01:00Z", "cli_version": "dev", "originator": "new-client", "source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent, "depth": 1}}}, "subagent_history_start_ordinal": count + 1}}
				header, _ := json.Marshal(meta)
				var raw strings.Builder
				raw.Write(header)
				raw.WriteByte('\n')
				for range count {
					raw.WriteString(`{"type":"turn_context","payload":{"model":"synthetic inherited model","synthetic_padding":"`)
					raw.WriteString(strings.Repeat("x", 128))
					raw.WriteString(`"}}`)
					raw.WriteByte('\n')
				}
				raw.WriteString(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"` + id + `","started_at":"2026-10-01T12:01:00Z"}}`)
				raw.WriteByte('\n')
				raw.WriteString(`{"type":"response_item","timestamp":"2026-10-01T12:01:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic own prompt"}]}}`)
				raw.WriteByte('\n')
				path := filepath.Join(home, "sessions", "rollout-"+id+".jsonl")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					b.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(raw.String()), 0600); err != nil {
					b.Fatal(err)
				}
				nativeBytes := raw.Len()
				raw.Reset()
				options := Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}
				decodes := state.PublishedStateLoads()
				b.StartTimer()
				health, err := runWithCensus(context.Background(), store, cfg, options, registeredAdapters())
				b.StopTimer()
				if err != nil || health.Registered != 1 {
					b.Fatalf("initial admission count=%d health=%+v err=%v", count, health, err)
				}
				b.ReportMetric(float64(nativeBytes), "native-file-bytes")
				b.ReportMetric(float64(health.NativeValidationBytes), "native-validation-bytes")
				b.ReportMetric(float64(health.NativeValidationReads), "native-read-calls")
				b.ReportMetric(float64(health.NativeValidationOpens), "native-opens")
				b.ReportMetric(float64(health.Probes), "header-probes")
				b.ReportMetric(float64(health.NativeValidationAttempts), "native-proof-attempts")
				b.ReportMetric(float64(state.PublishedStateLoads()-decodes), "full-published-decodes")
			}
		})
	}
}
