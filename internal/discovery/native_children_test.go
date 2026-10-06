package discovery

import (
	"context"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNativeChildAdmissionUsesOwnTaskBeyondHeaderWindowWithoutParent(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "own_task", true: "invalid_first_own"}[invalid], func(t *testing.T) {
			store, cfg, at, home := fixture(t)
			parent := "00000000-0000-0000-0000-000000000900"
			id, path := compatibilityRollout(t, home, cfg.Archive.Projects[0].Root, "codex-155-legacy.jsonl", "dev", 901, func(m, task map[string]any) {
				m["session_id"], m["parent_thread_id"] = parent, parent
				m["source"] = map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent, "depth": 1}}}
				m["subagent_history_start_ordinal"] = 1025
				if invalid {
					task["turn_id"] = "external-import-turn"
				}
			})
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(string(data), "\n")
			raw := lines[0] + "\n" + `{"type":"event_msg","payload":{"type":"task_started","turn_id":"` + parent + `","started_at":"2026-10-01T12:01:00Z"}}` + "\n"
			for range 1023 {
				raw += `{"type":"turn_context","payload":{"model":"synthetic inherited model","synthetic_padding":"` + strings.Repeat("x", 512) + `"}}` + "\n"
			}
			firstOwnByte := len(raw)
			if firstOwnByte <= sourcefacts.HeaderBytes {
				t.Fatalf("fixture does not exceed header byte window: %d", firstOwnByte)
			}
			t.Logf("first own task starts after %d bytes and 1025 raw records; header limits %d bytes/%d records", firstOwnByte, sourcefacts.HeaderBytes, sourcefacts.HeaderRecords)
			raw += strings.Join(lines[1:], "\n")
			if invalid {
				raw += `{"type":"event_msg","payload":{"type":"task_started","turn_id":"` + id + `","started_at":"2026-10-01T12:01:00Z"}}` + "\n"
			}
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			h, err := runWithCensus(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, registeredAdapters())
			if err != nil {
				t.Fatal(err)
			}
			regs, err := store.LoadRegistrations()
			if err != nil {
				t.Fatal(err)
			}
			if invalid {
				if len(regs) != 0 || h.Outcomes["own_task_unavailable"] == 0 {
					t.Fatalf("copied/later tasks licensed child: %+v %v", h, regs)
				}
				return
			}
			if len(regs) != 1 || h.Registered != 1 {
				t.Fatalf("independent child remained pending: %+v %v", h, regs)
			}
			t.Logf("initial native proof: opens=%d reads=%d actual native bytes=%d probes=%d", h.NativeValidationOpens, h.NativeValidationReads, h.NativeValidationBytes, h.Probes)
			reg := regs[0]
			if reg.NativeSessionID != id || !reg.NativeChild || reg.ParentNativeSessionID != parent || reg.ParentSessionID != "" || reg.Origin != archive.SessionOriginDiscovery || !reg.HookObservedAt.IsZero() || !reg.ImportBatch.IsZero() || reg.CodexBinding == nil || reg.CodexBinding.OwnStart == nil || *reg.CodexBinding.OwnStart != 1025 {
				t.Fatalf("native provenance lost: %+v", reg)
			}
			saved, _, err := config.Load(store.Home())
			if err != nil || !saved.CodexHistoryProtection {
				t.Fatal("writer fence missing", err)
			}
			encoded, _ := json.Marshal(h)
			if strings.Contains(string(encoded), parent) || strings.Contains(string(encoded), path) {
				t.Fatal("private native facts leaked in aggregate health")
			}
		})
	}
}
