package sourcefacts

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChildFirstOwnTaskPresenceAndBoundedPrefix(t *testing.T) {
	const parent = "22222222-2222-4222-8222-222222222222"
	for _, boundary := range []int{-1, 0, 3, HeaderRecords + 1} {
		t.Run(string(rune(boundary+66)), func(t *testing.T) {
			meta := map[string]any{"id": testID, "timestamp": "2026-10-01T12:00:00Z", "cwd": "/synthetic", "source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent, "depth": 1}}}, "originator": "new-client", "cli_version": "dev"}
			if boundary >= 0 {
				meta["subagent_history_start_ordinal"] = boundary
			}
			head, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": meta})
			own := strings.SplitN(testRecords(t, nil, nil), "\n", 2)[1]
			raw := string(head) + "\n"
			if boundary >= 3 {
				raw += `{"type":"event_msg","payload":{"type":"task_started","turn_id":"external-parent-task"}}` + "\n"
				for i := 2; i < boundary; i++ {
					raw += `{"type":"turn_context","payload":{"model":"inherited"}}` + "\n"
				}
			}
			raw += own
			h := ReadCodexHeader(strings.NewReader(raw), "rollout-"+testID+".jsonl")
			if boundary > HeaderRecords {
				if h.Outcome != "incomplete_metadata" || !h.FirstTaskAt.IsZero() {
					t.Fatalf("copied prefix licensed child: %+v", h)
				}
				return
			}
			if h.Outcome != "native_format" || h.FirstTaskAt.IsZero() || h.Identity == nil || !h.Identity.Child || !SupportedCodexProducer(h.Meta) {
				t.Fatalf("own child evidence rejected: %+v", h)
			}
			if (boundary < 0) != (h.Identity.SubagentOrdinal == nil) {
				t.Fatal("absent and zero collapsed")
			}
		})
	}
}

func TestUnknownChildSourceCannotClaimLocalExecution(t *testing.T) {
	for _, source := range []json.RawMessage{json.RawMessage(`"remote"`), json.RawMessage(`{"subagent":{"other":"custom"}}`), json.RawMessage(`{"unknown":{}}`)} {
		m := CodexMeta{Source: source, Parent: json.RawMessage(`"22222222-2222-4222-8222-222222222222"`), Originator: "new", Version: "dev"}
		if m.LocalExecutionSource() || SupportedCodexProducer(m) {
			t.Fatalf("unknown local source accepted: %s", source)
		}
	}
}

func TestChildExecutionFactsDropLargeAdditiveSourceContent(t *testing.T) {
	const parent = "22222222-2222-4222-8222-222222222222"
	meta := map[string]any{"id": testID, "timestamp": "2026-10-01T12:00:00Z", "cwd": "/synthetic", "source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent, "depth": 1, "additive_private_field": strings.Repeat("PRIVATE_UNKNOWN_SOURCE_FIELD", 1024)}}}, "originator": "new-client", "cli_version": "dev"}
	h := ReadCodexHeader(strings.NewReader(testRecords(t, meta, nil)), "rollout-"+testID+".jsonl")
	if h.Outcome != "native_format" || !SupportedCodexProducer(h.Meta) || len(h.Meta.Source) > 256 || h.FormatFacts == nil {
		t.Fatalf("bounded compatible source projection failed: %+v", h)
	}
	encoded, _ := json.Marshal(h)
	if strings.Contains(string(encoded), "PRIVATE_UNKNOWN_SOURCE_FIELD") {
		t.Fatal("unknown source text retained in bounded catalog facts")
	}
}
