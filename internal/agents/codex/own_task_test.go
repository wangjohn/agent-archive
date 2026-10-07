package codex

import (
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestSnapshotFirstOwnTaskSkipsLargeCopiedPrefixAndRejectsLaterRepair(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "invalid_first"}[invalid], func(t *testing.T) {
			dir := t.TempDir()
			boundary := uint64(1025)
			ref, _ := historyFile(t, dir, threadB, threadB, 0, map[string]any{"parent_thread_id": threadA, "session_id": threadA, "subagent_history_start_ordinal": boundary})
			task := func(id string) map[string]any {
				return map[string]any{"type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": id, "root_turn_id": id, "started_at": "2026-10-01T12:02:00Z"}}
			}
			rows := []map[string]any{task(threadA)}
			for range 1023 {
				rows = append(rows, map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "synthetic copied context"}}}})
			}
			first := threadB
			if invalid {
				first = "imported-turn"
			}
			rows = append(rows, task(first), task(threadB))
			appendHistoryRows(t, ref, 1, rows...)
			lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &ref, Revision: "one"}}
			pass := historyPass(t, dir, lookup)
			snap, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{RawBytes: 8 << 20, RecordBytes: archive.MaxRecordBytes})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = snap.Close() }()
			if err := snap.(agentapi.SourceAdmissionValidator).ValidateAdmission(t.Context(), agentapi.SourceAdmission{NativeID: threadB, Cwd: dir}); err != nil {
				t.Fatal(err)
			}
			facts, err := snap.(agentapi.SourceOwnTaskFacts).FirstOwnTask(t.Context())
			if err != nil || !facts.Seen || facts.Native == invalid || !facts.LocalExecution || facts.TurnID != first {
				t.Fatalf("owned first task %+v %v", facts, err)
			}
			lookup.changed = true
			if _, err := snap.(agentapi.SourceOwnTaskFacts).FirstOwnTask(t.Context()); !agentapi.HasFailure(err, agentapi.Changed) {
				t.Fatalf("cached own facts ignored changed selection: %v", err)
			}
		})
	}
}
