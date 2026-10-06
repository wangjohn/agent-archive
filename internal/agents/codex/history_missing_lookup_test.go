package codex

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"os"
	"path/filepath"
	"testing"
)

func TestRelatedRevisionWithoutLookupStaysPending(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ref, _ := historyFile(t, dir, rolloutC, threadA, 0, nil, "own")
	p, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	if _, err := p.Signature(t.Context(), ref); err == nil || agentapi.Failure(err) != agentapi.Unavailable {
		t.Errorf("signature without current evidence: %v", err)
	}
	snap, err := p.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err == nil {
		defer func() { _ = snap.Close() }()
		filtered, filterErr := (Filter{}).Filter(t.Context(), snap.Input(), agentapi.FilterContext{})
		t.Fatalf("read without current evidence succeeded: history=%v records=%d filter=%v", filtered.History, len(filtered.Records), filterErr)
	}
	if agentapi.Failure(err) != agentapi.Unavailable {
		t.Fatalf("pending evidence: %v", err)
	}
	owned := p.(*relatedSourcePass)
	if owned.bytes != 0 || len(owned.files) != 0 {
		t.Fatal("missing evidence retained unused resources")
	}
}

func TestMissingLookupPreservesOrdinaryAndGenericCompatibility(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ordinary, raw := historyFile(t, dir, threadA, threadA, 0, nil, "own")
	generic := agentapi.SourceRef{Path: filepath.Join(dir, "legacy.jsonl")}
	if err := os.WriteFile(generic.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	p, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	for _, ref := range []agentapi.SourceRef{ordinary, generic} {
		if _, err := p.Signature(t.Context(), ref); err != nil {
			t.Fatal(err)
		}
		snap, err := p.Read(t.Context(), ref, agentapi.ReadLimits{})
		if err != nil {
			t.Fatal(err)
		}
		filtered, filterErr := (Filter{}).Filter(t.Context(), snap.Input(), agentapi.FilterContext{Filename: filepath.Base(ref.Path)})
		closeErr := snap.Close()
		if filterErr != nil || closeErr != nil || filtered.History != nil || len(filtered.Records) != 2 {
			t.Fatalf("ordinary compatibility: history=%v records=%d filter=%v close=%v", filtered.History, len(filtered.Records), filterErr, closeErr)
		}
	}
}
