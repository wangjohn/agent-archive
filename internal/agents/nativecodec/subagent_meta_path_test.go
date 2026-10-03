package nativecodec

import "testing"

func TestSubagentMetaPathPreservesSpelling(t *testing.T) {
	for _, path := range []string{
		"agent-child.jsonl",
		"./agent-child.jsonl",
		"relative/link/../agent-child.jsonl",
		"/synthetic//link/../agent-child.jsonl",
		"/synthetic/./agent-子.jsonl",
	} {
		got, ok := SubagentMetaPath(path)
		want := path[:len(path)-len(".jsonl")] + ".meta.json"
		if !ok || got != want {
			t.Errorf("SubagentMetaPath(%q) = %q, %v; want %q, true", path, got, ok, want)
		}
	}
	for _, path := range []string{"", "agent-.jsonl", "child.jsonl", "agent-child.jsonl.extra", "agent-child.jsonl/", "agent-child.meta.json"} {
		if got, ok := SubagentMetaPath(path); ok || got != "" {
			t.Errorf("SubagentMetaPath(%q) = %q, %v; want empty, false", path, got, ok)
		}
	}
}
