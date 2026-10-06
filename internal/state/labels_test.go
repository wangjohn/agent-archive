package state

import (
	"os"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestLabelCacheRejectsUnfilteredContext(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entry := LabelEntry{Scope: strings.Repeat("a", 64), SourceChecksum: strings.Repeat("b", 64), Context: agentapi.LabelContext{NativeID: "01900000-0000-7000-8000-000000000001", Producer: "0.159.2", Contract: strings.Repeat("c", 64)}}
	cache := LabelCache{Version: 1, Entries: map[string]LabelEntry{"synthetic": entry}}
	if err := store.SaveLabels(cache); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*LabelEntry){
		func(e *LabelEntry) { e.Context.Producer = "private metadata" },
		func(e *LabelEntry) { e.Context.Contract += "/private metadata" },
		func(e *LabelEntry) { e.Context.PreviewDigest = "private prompt" },
		func(e *LabelEntry) { e.SourceChecksum = "private path" },
	} {
		bad := entry
		mutate(&bad)
		cache.Entries["synthetic"] = bad
		if store.SaveLabels(cache) == nil {
			t.Fatal("arbitrary context string entered cache")
		}
	}
}

func TestLabelContextReadBudgetRejectsOversizeBeforeDecode(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.publishedPath("synthetic"), make([]byte, 1024), 0600); err != nil {
		t.Fatal(err)
	}
	if _, n, err := store.LoadLabelPublication("synthetic", 512); err == nil || n != 0 {
		t.Fatal("over-budget retained source was read")
	}
}
