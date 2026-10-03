package collector

import (
	"fmt"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestRefreshSkipCountUsesRegisteredParserVersions(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	bindings := &operationBindings{parser: &operationParser{version: "codex-current"}}
	p := pass{local: store, opts: Options{Parsers: bindings, parserCache: make(map[string]agentapi.TranscriptParser)}}
	for i := range 1000 {
		p.registrations = append(p.registrations, archive.SessionRegistration{ArchiveSessionID: fmt.Sprintf("absent-%d", i), Harness: archive.Harness{Name: "codex"}})
	}
	if got := p.countRefreshSkips(); got != 0 || bindings.lookups != 0 {
		t.Fatalf("absent skips: count=%d lookups=%d", got, bindings.lookups)
	}
	p.registrations = append(p.registrations,
		archive.SessionRegistration{ArchiveSessionID: "current", Harness: archive.Harness{Name: "codex"}},
		archive.SessionRegistration{ArchiveSessionID: "stale", Harness: archive.Harness{Name: "codex"}},
		archive.SessionRegistration{ArchiveSessionID: "unavailable", Harness: archive.Harness{Name: "claude"}})
	for id, version := range map[string]string{"current": "codex-current", "stale": "old", "unavailable": "unavailable", "orphan": "codex-current"} {
		if err := store.SaveRefreshSkip(id, state.RefreshSkip{ParserVersion: version}); err != nil {
			t.Fatal(err)
		}
	}
	if got := p.countRefreshSkips(); got != 2 {
		t.Fatalf("registered matching skips=%d, want 2", got)
	}
	if bindings.lookups != 2 {
		t.Fatalf("lookups=%d, want once per registered agent", bindings.lookups)
	}
	p.opts.ParserVersion = "old"
	if got := p.countRefreshSkips(); got != 1 {
		t.Fatalf("override skips=%d, want 1", got)
	}
}
