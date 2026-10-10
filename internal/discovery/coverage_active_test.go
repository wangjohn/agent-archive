package discovery

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

// changingCoverageAdapter changes native files each time sessions/ is
// enumerated from its start, so every change lands between an epoch's
// observation and its validation, as a running Codex session's appends do.
type changingCoverageAdapter struct {
	SourceAdapter
	change func(call int)
	calls  int
}

func (a *changingCoverageAdapter) Enumerate(ctx context.Context, root, path string, cookie int64) (SourceBatch, error) {
	if path == "sessions" && cookie == 0 {
		a.calls++
		a.change(a.calls)
	}
	return a.SourceAdapter.Enumerate(ctx, root, path, cookie)
}

func rolloutPath(root, folder, id string) string {
	return filepath.Join(root, folder, "rollout-2026-10-01T12-00-00-"+id+".jsonl")
}

func appendRolloutLine(tb testing.TB, path string) {
	tb.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"event_msg","payload":{"type":"token_count"}}` + "\n"); err != nil {
		tb.Fatal(err)
	}
	if err := f.Close(); err != nil {
		tb.Fatal(err)
	}
}

type coverageFixture struct {
	store *state.Store
	cfg   config.Config
	at    time.Time
	root  string
}

// requestThread enrolls a thread's coverage the way a registered capture does.
func (f coverageFixture) requestThread(t *testing.T, id string) {
	t.Helper()
	lookup, err := NewCodexRolloutLookup(t.Context(), f.store, []string{f.root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lookup.Thread(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := lookup.Close(); err != nil {
		t.Fatal(err)
	}
}

// pass runs one scheduled discovery pass and reads the thread's coverage.
func (f coverageFixture) pass(t *testing.T, n int, adapter SourceAdapter, id string) (bool, []string) {
	t.Helper()
	lookup, err := NewCodexRolloutLookup(t.Context(), f.store, []string{f.root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runWithAdapters(context.Background(), f.store, f.cfg, Options{Now: func() time.Time { return f.at.Add(time.Duration(n) * time.Minute) }, Rollouts: lookup}, []SourceAdapter{adapter}); err != nil {
		t.Fatal(err)
	}
	set, err := lookup.Thread(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, candidate := range set.Candidates {
		paths = append(paths, candidate.Path)
	}
	if err := lookup.Close(); err != nil {
		t.Fatal(err)
	}
	return set.Complete, paths
}

// A running Codex session appends to its rollout continuously. That must not
// keep every other thread's history coverage from ever completing, whether the
// requested rollout was archived before it was requested or moved from
// sessions/ to archived_sessions/ afterwards.
func TestCoverageCompletesWhileAnotherRolloutIsAppended(t *testing.T) {
	t.Parallel()
	for _, movedAfterRequest := range []bool{false, true} {
		t.Run(map[bool]string{false: "archived before request", true: "archived after request"}[movedAfterRequest], func(t *testing.T) {
			t.Parallel()
			store, cfg, at, root := fixture(t)
			f := coverageFixture{store: store, cfg: cfg, at: at, root: root}
			project := cfg.Archive.Projects[0].Root
			folder := "archived_sessions"
			if movedAfterRequest {
				folder = "sessions"
			}
			wanted := writeRollout(t, root, project, at.Add(-2*time.Hour), 1, folder)
			active := writeRollout(t, root, project, at.Add(-time.Hour), 2, "sessions")
			f.requestThread(t, wanted)
			archived := rolloutPath(root, "archived_sessions", wanted)
			if movedAfterRequest {
				if err := os.MkdirAll(filepath.Dir(archived), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(rolloutPath(root, "sessions", wanted), archived); err != nil {
					t.Fatal(err)
				}
			}
			adapter := &changingCoverageAdapter{SourceAdapter: codexAdapter{supported: syntheticSupport}, change: func(int) { appendRolloutLine(t, rolloutPath(root, "sessions", active)) }}
			for n := range 4 {
				complete, candidates := f.pass(t, n, adapter, wanted)
				if !complete {
					continue
				}
				if !slices.Equal(candidates, []string{archived}) {
					t.Fatalf("candidates %v, want only the archived rollout", candidates)
				}
				return
			}
			t.Fatal("coverage never completed while another rollout was being appended")
		})
	}
}

// Membership digests no longer include rollout sizes and times, so an
// unrelated rollout rewritten in place to claim the requested thread between
// observation and validation must still fail that epoch.
func TestCoverageRejectsRolloutRewrittenToRequestedThreadMidEpoch(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	f := coverageFixture{store: store, cfg: cfg, at: at, root: root}
	project := cfg.Archive.Projects[0].Root
	wanted := writeRollout(t, root, project, at.Add(-2*time.Hour), 1, "archived_sessions")
	other := writeRollout(t, root, project, at.Add(-time.Hour), 2, "sessions")
	claim, err := os.ReadFile(rolloutPath(root, "archived_sessions", wanted))
	if err != nil {
		t.Fatal(err)
	}
	f.requestThread(t, wanted)
	otherPath := rolloutPath(root, "sessions", other)
	before, err := os.Stat(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := false
	adapter := &changingCoverageAdapter{SourceAdapter: codexAdapter{supported: syntheticSupport}, change: func(call int) {
		// The first enumeration observes; the second validates.
		if call == 2 {
			if err := os.WriteFile(otherPath, claim, 0600); err != nil {
				t.Fatal(err)
			}
			rewritten = true
		}
	}}
	complete, candidates := f.pass(t, 0, adapter, wanted)
	if !rewritten {
		t.Fatal("fixture did not rewrite during validation")
	}
	after, err := os.Stat(otherPath)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("fixture replaced the file instead of rewriting it in place")
	}
	if complete {
		t.Fatalf("certified a thread inventory missing a rewritten member: %v", candidates)
	}
	for n := 1; n < 4; n++ {
		complete, candidates = f.pass(t, n, adapter, wanted)
		if complete {
			want := []string{rolloutPath(root, "archived_sessions", wanted), otherPath}
			slices.Sort(want)
			if !slices.Equal(candidates, want) {
				t.Fatalf("candidates %v, want %v", candidates, want)
			}
			return
		}
	}
	t.Fatal("coverage did not converge after the rewrite")
}

// Codex archives a rollout by renaming it from sessions/ to archived_sessions/.
// The observation cache keeps the old path's identity, and a lookup restores
// cached facts into an epoch that is still observing. Complete coverage must
// list only rollouts validation found, never the vanished pre-move locator.
func TestCoverageDropsCachedCandidateMovedOutOfSessions(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	f := coverageFixture{store: store, cfg: cfg, at: at, root: root}
	wanted := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(-2*time.Hour), 1, "sessions")
	f.requestThread(t, wanted)
	adapter := codexAdapter{supported: syntheticSupport}
	n := 0
	for ; n < 4; n++ {
		if complete, _ := f.pass(t, n, adapter, wanted); complete {
			break
		}
	}
	if n == 4 {
		t.Fatal("baseline coverage never completed")
	}
	archived := rolloutPath(root, "archived_sessions", wanted)
	if err := os.MkdirAll(filepath.Dir(archived), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(rolloutPath(root, "sessions", wanted), archived); err != nil {
		t.Fatal(err)
	}
	// A pass stopped before observing anything leaves the new epoch observing,
	// so the next lookup restores the cached pre-move fact into it.
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runWithAdapters(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(10 * time.Minute) }, Rollouts: lookup, Stop: func() bool { return true }}, []SourceAdapter{adapter}); err != nil {
		t.Fatal(err)
	}
	if err := lookup.Close(); err != nil {
		t.Fatal(err)
	}
	for n = 11; n < 15; n++ {
		complete, candidates := f.pass(t, n, adapter, wanted)
		if !complete {
			continue
		}
		if !slices.Equal(candidates, []string{archived}) {
			t.Fatalf("candidates %v, want only the archived rollout", candidates)
		}
		return
	}
	t.Fatal("coverage never completed after the move")
}

// Re-reading a changed or uncached rollout's header spends the pass's probe
// budget. A validation batch holding more such rollouts than one pass can
// probe must still finish across passes instead of rereading the same prefix.
func TestCoverageMemberChecksProgressAcrossProbeLimitedPasses(t *testing.T) {
	t.Parallel()
	_, cfg, at, root := fixture(t)
	project := cfg.Archive.Projects[0].Root
	for n := 1; n <= 150; n++ {
		writeRollout(t, root, project, at.Add(-time.Hour), n, "archived_sessions")
	}
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	c := newCoverage([]string{root})
	c.request("requested")
	c.restart()
	collectCoverageDirectory(t, c, root, "sessions", false)
	collectCoverageDirectory(t, c, root, "archived_sessions", false)
	c.beginValidation()
	// An empty observation cache stands for entries evicted or changed since
	// observation; each pass leaves room for only 100 header probes.
	cat := &catalog{Coverage: c, Cache: map[string]cached{}}
	for pass := range 4 {
		h := &Health{Probes: HeaderProbes - 100}
		advanceCoverageValidation(t.Context(), cat, h, codexAdapter{}, time.Now().Add(time.Minute), Options{})
		if c.Phase == coverageComplete {
			if c.Failed || pass == 0 {
				t.Fatalf("pass %d failed=%v", pass, c.Failed)
			}
			return
		}
	}
	t.Fatal("validation never finished under a per-pass probe limit")
}
