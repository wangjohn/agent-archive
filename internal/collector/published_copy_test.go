package collector

import (
	"context"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/storage"
)

// One scan both refreshes a session's metadata for a new parser and
// publishes its grown transcript, over state written before either the
// source reference or the metadata document was cached. The refresh reads
// the live metadata from the bucket and caches it in the published state;
// the publication after it must see that update through the scan's one
// in-memory copy, and name the object the live metadata points at as the
// one it supersedes. From a stale copy it would find no reference at all.
func TestParserUpgradeAndPublicationInOneScanOverLegacyState(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	firstKey := publishThenGrow(t, local, store, t0)
	editPublishedState(t, local, func(state map[string]any) {
		withoutRecordedSource(state)
		delete(state, "metadata_bytes")
	})
	now := t0.Add(time.Hour)
	result, err := Run(context.Background(), local, store, Options{MachineID: "m", ParserVersion: "upgraded", Now: func() time.Time { return now }})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("scan: %#v %v", result, err)
	}
	superseded, err := local.LoadSuperseded("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(superseded) != 1 || superseded[0].Key != firstKey {
		t.Fatalf("superseded = %#v, want only %s", superseded, firstKey)
	}
	current := fetchMetadata(t, store, "codex", "session-1")
	if current.Parser.Version != "upgraded" || current.SourceBundle.Key == firstKey {
		t.Fatalf("metadata = parser %q, source %s", current.Parser.Version, current.SourceBundle.Key)
	}
	if source, found, err := local.LoadLastPublishedSource("session-1"); err != nil || !found || source != current.SourceBundle {
		t.Fatalf("recorded source = %#v %v %v, want %#v", source, found, err, current.SourceBundle)
	}
}
