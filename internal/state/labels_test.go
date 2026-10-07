package state

import (
	"fmt"
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

func TestLabelTargetCursorRejectsRawGroupsAndUnboundedState(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cache := LabelCache{Version: 1, Entries: map[string]LabelEntry{}, TargetCursors: map[string]string{strings.Repeat("a", 64): "admitted-id"}}
	if err := store.SaveLabels(cache); err != nil {
		t.Fatal(err)
	}
	for _, cursors := range []map[string]string{{"/private/native/home": "admitted-id"}, {strings.Repeat("a", 64): "../foreign"}, {strings.Repeat("a", 64): strings.Repeat("x", 257)}} {
		cache.TargetCursors = cursors
		if store.SaveLabels(cache) == nil {
			t.Fatal("invalid cursor persisted")
		}
	}
	cache.TargetCursors = map[string]string{}
	for i := range MaxLabelTargetCursors + 1 {
		cache.TargetCursors[fmt.Sprintf("%064x", i)] = "admitted-id"
	}
	if store.SaveLabels(cache) == nil {
		t.Fatal("over-budget cursors persisted")
	}
}

func TestLabelPublicationUsesSharedReadBudgetAndReleasesOwnership(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.publishedPath("synthetic"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(3)
	scoped, closeScope := store.WithReadBudget(t.Context(), budget)
	before := PublishedStateLoads()
	if _, n, err := scoped.LoadLabelPublication("synthetic", 512); err == nil || n != 0 || PublishedStateLoads() != before {
		t.Fatal("label decode bypassed shared serialized-plus-decoded pressure")
	}
	closeScope()
	if budget.Available() != 3 {
		t.Fatal("refused label read leaked charge")
	}
	budget = agentapi.NewNativeReadBudget(4)
	scoped, closeScope = store.WithReadBudget(t.Context(), budget)
	if _, n, err := scoped.LoadLabelPublication("synthetic", 512); err != nil || n != 2 || budget.Available() != 2 {
		t.Fatal("decoded label state was not charged", n, err, budget.Available())
	}
	closeScope()
	closeScope()
	if budget.Available() != 4 {
		t.Fatal("label scope did not release decoded ownership")
	}
}

func TestLabelRevisionDoesNotDecodeOversizedLegacyPublication(t *testing.T) {
	store := newTestStore(t)
	if err := os.WriteFile(store.publishedPath("synthetic"), []byte("{\"bundle\":"+strings.Repeat(" ", 8<<20)), 0600); err != nil {
		t.Fatal(err)
	}
	before := PublishedStateLoads()
	if _, stamp, err := store.LabelRevision("synthetic"); err != nil || stamp == "" {
		t.Fatal("legacy stat proof unavailable", err)
	}
	if _, n, err := store.LoadLabelPublication("synthetic", 16<<20); err == nil || n != 0 || PublishedStateLoads() != before {
		t.Fatal("missing leading summary bypassed capped publication decode")
	}
}
