package codex

import (
	"bytes"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"os"
	"testing"
	"time"
)

type captureTimestampScenario string

const (
	captureTimestampOtherThread captureTimestampScenario = "other_thread"
	captureTimestampChildCopied captureTimestampScenario = "child_copied"
	captureTimestampChildZero   captureTimestampScenario = "child_zero"
	captureTimestampChildAbsent captureTimestampScenario = "child_absent"
	captureTimestampForkCopied  captureTimestampScenario = "fork_copied"
	captureTimestampRevert      captureTimestampScenario = "revert"
)

func TestHistoryCaptureFactsRespectRecordOwnership(t *testing.T) {
	t.Parallel()
	for _, kind := range []captureTimestampScenario{captureTimestampOtherThread, captureTimestampChildCopied, captureTimestampChildZero, captureTimestampChildAbsent, captureTimestampForkCopied, captureTimestampRevert} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			for _, timestamp := range []string{"", "2026-10-02T12:00:00Z"} {
				t.Run(timestamp, func(t *testing.T) {
					t.Parallel()
					dir := t.TempDir()
					base, baseRaw := historyFile(t, dir, threadA, threadA, 0, nil)
					writeTimestampedHeader(t, base, baseRaw)
					row := map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "prefix"}}}}
					if timestamp != "" {
						row["timestamp"] = timestamp
					}
					raw := appendHistoryRows(t, base, 1, row)
					thread := threadA
					extra := map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}}
					switch kind {
					case captureTimestampOtherThread:
						thread = threadB
						extra["parent_thread_id"] = threadA
					case captureTimestampChildCopied:
						extra["parent_thread_id"] = threadB
						extra["subagent_history_start_ordinal"] = 2
					case captureTimestampChildZero:
						extra["parent_thread_id"] = threadB
						extra["subagent_history_start_ordinal"] = 0
					case captureTimestampChildAbsent:
						extra["parent_thread_id"] = threadB
					case captureTimestampForkCopied:
						extra["forked_from_id"] = threadB
						extra["forked_from_ordinal_exclusive"] = 2
					case captureTimestampRevert:
					}
					leaf, leafRaw := historyFile(t, dir, rolloutC, thread, 2, extra, "own")
					writeTimestampedHeader(t, leaf, leafRaw)
					lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
					snap, err := historyPass(t, dir, lookup).Read(t.Context(), leaf, agentapi.ReadLimits{})
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = snap.Close() }()
					filtered, err := (Filter{}).Filter(t.Context(), snap.Input(), agentapi.FilterContext{})
					if err != nil {
						t.Fatal(err)
					}
					ownPrefix := kind == captureTimestampChildZero || kind == captureTimestampChildAbsent || kind == captureTimestampRevert
					wantEnd := time.Date(2026, 10, 1, 12, 1, 0, 0, time.UTC)
					if ownPrefix && timestamp != "" {
						wantEnd = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
					}
					wantComplete := !ownPrefix || timestamp != ""
					if !filtered.NativeEndAt.Equal(wantEnd) || filtered.NativeStartComplete != wantComplete {
						t.Fatalf("capture facts: end=%s complete=%v; want end=%s complete=%v", filtered.NativeEndAt, filtered.NativeStartComplete, wantEnd, wantComplete)
					}
					if len(filtered.Records) != 4 || len(filtered.Ordinals) != 4 || filtered.History.Spans[0].EndRecord != 2 {
						t.Fatal("ownership changed retained prefix coverage")
					}
				})
			}
		})
	}
}

func writeTimestampedHeader(t *testing.T, ref agentapi.SourceRef, raw []byte) {
	t.Helper()
	end := bytes.IndexByte(raw, '\n')
	var header map[string]any
	if err := json.Unmarshal(raw[:end], &header); err != nil {
		t.Fatal(err)
	}
	header["timestamp"] = "2026-10-01T12:00:00Z"
	encoded, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ref.Path, append(encoded, raw[end:]...), 0600); err != nil {
		t.Fatal(err)
	}
}
