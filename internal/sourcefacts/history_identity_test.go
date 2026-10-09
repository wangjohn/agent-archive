package sourcefacts

import (
	"strings"
	"testing"
)

func TestUnderstoodChildHeaderProvidesOwnTaskAndLookupEvidence(t *testing.T) {
	id := testID
	parent := "22222222-2222-4222-8222-222222222222"
	meta := map[string]any{"id": id, "timestamp": "2026-10-01T12:00:00Z", "cwd": "/synthetic", "source": "cli", "originator": "test", "cli_version": "0.160.0", "parent_thread_id": parent, "subagent_history_start_ordinal": 0}
	h := ReadCodexHeader(strings.NewReader(testRecords(t, meta, nil)), "rollout-"+id+".jsonl")
	if h.Outcome != "native_format" || h.Meta.ID != id || h.FirstTaskAt.IsZero() {
		t.Fatalf("pending facts became admission: %#v", h)
	}
	if h.Identity == nil || h.Identity.ThreadID != id || h.Identity.ParentID != parent || h.Identity.SubagentOrdinal == nil || *h.Identity.SubagentOrdinal != 0 || h.NativeCreatedAt.IsZero() {
		t.Fatalf("bounded lookup identity was lost: %#v", h)
	}
	meta["parent_thread_id"] = id
	h = ReadCodexHeader(strings.NewReader(testRecords(t, meta, nil)), "rollout-"+id+".jsonl")
	if h.Identity != nil {
		t.Fatal("malformed relationship retained lookup authority")
	}
	delete(meta, "parent_thread_id")
	delete(meta, "subagent_history_start_ordinal")
	meta["source"] = "unknown_execution"
	h = ReadCodexHeader(strings.NewReader(testRecords(t, meta, nil)), "rollout-"+id+".jsonl")
	if h.Identity != nil {
		t.Fatal("unknown execution retained lookup authority")
	}
}
