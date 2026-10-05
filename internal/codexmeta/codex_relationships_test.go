package codexmeta

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
)

const thread = "00000000-0000-0000-0000-000000000003"

const root = "00000000-0000-0000-0000-000000000001"

const parent = "00000000-0000-0000-0000-000000000002"

const segment = "00000000-0000-0000-0000-000000000004"

func metadata(t *testing.T, fields string) CodexMeta {
	t.Helper()
	var m CodexMeta
	raw := `{"id":"` + thread + `","cwd":"/synthetic/project","source":"cli","originator":"synthetic","cli_version":"future+build"` + fields + `}`
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCodexIdentitiesAndRelationships(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		fields  string
		rollout string
		outcome Outcome
		child   bool
		root    string
		parent  string
		fork    string
	}{
		{name: "legacy", rollout: thread, outcome: NativeFormat},
		{name: "root", fields: `,"session_id":"` + thread + `","unrecognized":{"private":"discard"}`, rollout: thread, outcome: NativeFormat, root: thread},
		{name: "child", fields: `,"session_id":"` + root + `","parent_thread_id":"` + root + `"`, rollout: thread, outcome: ChildHistoryPending, child: true, root: root, parent: root},
		{name: "grandchild", fields: `,"session_id":"` + root + `","parent_thread_id":"` + parent + `","source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + parent + `","depth":2,"agent_path":"/root/child/grandchild","extra":true}}}`, rollout: thread, outcome: ChildHistoryPending, child: true, root: root, parent: parent},
		{name: "legacy-source-parent", fields: `,"source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + parent + `","depth":1}}}`, rollout: thread, outcome: ChildHistoryPending, child: true, parent: parent},
		{name: "fork-is-independent", fields: `,"session_id":"` + thread + `","forked_from_id":"` + root + `","forked_from_ordinal_exclusive":0`, rollout: thread, outcome: ForkHistoryPending, root: thread, fork: root},
		{name: "revert", fields: `,"session_id":"` + thread + `","history_mode":"paginated","history_base":{"thread_id":"` + thread + `","end_ordinal_exclusive":0,"end_byte_offset":0,"future":true}`, rollout: segment, outcome: RelatedHistoryPending, root: thread},
		{name: "different-rollout-without-prefix", rollout: segment, outcome: RelatedHistoryPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := metadata(t, tc.fields)
			path := "rollout-" + tc.rollout + ".jsonl"
			f, code := m.Identity(path)
			if code != "" || f.ThreadID != thread || f.RolloutID != tc.rollout || f.RootID != tc.root || f.ParentID != tc.parent || f.ForkID != tc.fork || f.Child != tc.child || m.CaptureOutcome(path) != tc.outcome {
				t.Fatalf("facts=%+v identity=%s capture=%s", f, code, m.CaptureOutcome(path))
			}
			if tc.name == "fork-is-independent" && (f.ForkOrdinal == nil || *f.ForkOrdinal != 0 || f.SubagentOrdinal != nil) {
				t.Fatal("zero and absence conflated")
			}
			if tc.name == "revert" && (f.HistoryBase == nil || f.HistoryBase.RolloutID != thread || f.HistoryBase.EndOrdinal != 0 || f.HistoryBase.EndByteOffset != 0) {
				t.Fatal("prefix bounds lost")
			}
		})
	}
}

func TestCodexMalformedAndContradictoryRelationships(t *testing.T) {
	t.Parallel()
	for _, fields := range []string{
		`,"parent_thread_id":42`, `,"forked_from_id":{}`, `,"forked_from_id":"` + thread + `"`,
		`,"parent_thread_id":"` + thread + `"`, `,"session_id":"` + thread + `","parent_thread_id":"` + parent + `"`,
		`,"parent_thread_id":"` + parent + `","source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + root + `","depth":2}}}`,
		`,"forked_from_ordinal_exclusive":0`, `,"subagent_history_start_ordinal":-1`, `,"subagent_history_start_ordinal":1.5`,
		`,"history_base":{"thread_id":"` + root + `","end_ordinal_exclusive":1}`, `,"history_base":true`,
		`,"history_base":{"thread_id":"` + thread + `","end_ordinal_exclusive":0,"end_byte_offset":0}`,
		`,"source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + parent + `","depth":"2"}}}`,
		`,"agent_role":"one","agent_type":"two"`,
	} {
		t.Run(fields, func(t *testing.T) {
			t.Parallel()
			m := metadata(t, fields)
			if got := m.CaptureOutcome("rollout-" + thread + ".jsonl"); got != InvalidRelationship {
				t.Fatalf("got %s", got)
			}
		})
	}
	for _, fields := range []string{`,"thread_source":42`, `,"agent_path":{}`, `,"source":[]`} {
		m := metadata(t, fields)
		if got := m.CaptureOutcome("rollout-" + thread + ".jsonl"); got != InvalidMetadata {
			t.Fatalf("got %s", got)
		}
	}
	for _, fields := range []string{`,"history_mode":"compressed"`, `,"thread_source":"future_task"`, `,"source":{"subagent":{"future":{}}}`} {
		if got := metadata(t, fields).CaptureOutcome("rollout-" + thread + ".jsonl"); !strings.HasPrefix(string(got), "unsupported_") {
			t.Fatalf("unknown semantic shape: %s", got)
		}
	}
}

func BenchmarkCodexIdentity(b *testing.B) {
	const raw = `{"id":"00000000-0000-0000-0000-000000000003","session_id":"00000000-0000-0000-0000-000000000001","parent_thread_id":"00000000-0000-0000-0000-000000000002","cwd":"/synthetic/project","source":{"subagent":{"thread_spawn":{"parent_thread_id":"00000000-0000-0000-0000-000000000002","depth":2}}},"history_mode":"paginated","subagent_history_start_ordinal":0}`
	for _, count := range []int{1000, 10000, 100000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(count * len(raw)))
			b.ReportMetric(float64(count), "records/op")
			for b.Loop() {
				for range count {
					var m CodexMeta
					if err := json.Unmarshal([]byte(raw), &m); err != nil {
						b.Fatal(err)
					}
					f, code := m.Identity("rollout-" + thread + ".jsonl")
					if code != "" || !f.Child || f.SubagentOrdinal == nil {
						b.Fatal("identity decode failed")
					}
				}
			}
		})
	}
}
