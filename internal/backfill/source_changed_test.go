package backfill

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

type changedCodexSources struct{ agentapi.SourcesLookup }

func (s changedCodexSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	p, f, ok := s.SourcesLookup.LookupSources(name)
	if name == "codex" {
		f = changedCodexFilter{f}
	}
	return p, f, ok
}

type changedCodexFilter struct{ agentapi.TranscriptFilter }

func (changedCodexFilter) Filter(context.Context, agentapi.NativeInput, agentapi.FilterContext) (archive.FilteredTranscript, error) {
	return archive.FilteredTranscript{}, agentapi.Wrap(agentapi.Changed, errors.New("synthetic concurrent append"))
}

// A live source's failed snapshot check is a retryable observation, not corrupt
// JSON. Other sessions can be imported and a settled rerun can read the source.
func TestChangedSourceDoesNotBecomeUnsafeFormat(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	repo := tr.repo("home/repo")
	start := fixedNow.Add(-time.Hour)
	const id = "00000000-0000-0000-0000-000000000001"
	tr.write(filepath.Join("home", codexFile(id)), codexTranscript(id, id, repo, start))
	tr.write(filepath.Join("home", claudeFile("repo", "ordinary")), claudeTranscript("ordinary", repo, start))
	env := tr.env()
	env.Sources = changedCodexSources{env.Sources}
	p := plan(t, env, nil, config.Config{}, Filters{})
	if candidate(t, p, id).Skip != SkipSourceChanged || candidate(t, p, "ordinary").Skip != "" {
		t.Fatalf("changed-source plan: %+v", p.Candidates)
	}
	p = plan(t, tr.env(), nil, config.Config{}, Filters{})
	if c := candidate(t, p, id); c.Skip != "" {
		t.Fatalf("settled source remained blocked: %+v", c)
	}
}

// A later metadata record is outside the first-header observation, but still
// stops every physical copy of the thread from being selected as complete.
func TestLaterRelatedMetadataKeepsOrdinarySiblingPending(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	repo := tr.repo("home/repo")
	start := fixedNow.Add(-time.Hour)
	const id = "00000000-0000-0000-0000-000000000001"
	const parent = "00000000-0000-0000-0000-000000000002"
	ordinary := codexTranscript(id, id, repo, start)
	tr.write(filepath.Join("home", codexFile(id)), ordinary)
	later := `{"type":"session_meta","payload":{"id":"` + id + `","parent_thread_id":"` + parent + `"}}` + "\n"
	tr.write(filepath.Join("home", ".codex", "archived_sessions", "rollout-older-"+id+".jsonl"), ordinary+later)
	tr.write(filepath.Join("home", claudeFile("repo", "ordinary")), claudeTranscript("ordinary", repo, start))
	p := plan(t, tr.env(), nil, config.Config{}, Filters{})
	for _, c := range p.Candidates {
		if c.Harness == "codex" && c.Skip != SkipRelatedHistory && c.Skip != SkipDuplicateSession {
			t.Fatalf("sibling admitted: %+v", c)
		}
	}
	if candidate(t, p, "ordinary").Skip != "" {
		t.Fatal("unrelated candidate stopped")
	}
}
