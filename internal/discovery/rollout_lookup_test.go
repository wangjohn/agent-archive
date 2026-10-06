package discovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

func TestRolloutLookupCurrentWALIdentityRefreshAndPrivateCleanup(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	id := writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 1, "sessions")
	path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+id+".jsonl")
	other := writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 2, "sessions")
	otherPath := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+other+".jsonl")
	raw, err := os.ReadFile(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.ReplaceAll(string(raw), other, id))
	if err := os.WriteFile(otherPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	db := hintDatabase(t, root, true)
	addHint(t, db, id, path, at)
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lookup.Close() }()
	set, err := lookup.Thread(t.Context(), id)
	if err != nil || set.Current == nil || set.Current.Path != path || set.Complete {
		t.Fatal(set, err)
	}
	if err := lookup.Check(t.Context(), id, set.Revision); err != nil {
		t.Fatal(err)
	}
	// Unrelated commits do not invalidate the current locator token.
	addHint(t, db, "unrelated", "unused", at.Add(time.Minute))
	if err := lookup.Check(t.Context(), id, set.Revision); err != nil {
		t.Fatal(err)
	}
	// One refresh is already consumed. A later changed selection stays pending.
	if _, err := db.ExecContext(t.Context(), "UPDATE threads SET rollout_path=? WHERE id=?", otherPath, id); err != nil {
		t.Fatal(err)
	}
	if err := lookup.Check(t.Context(), id, set.Revision); agentapi.Failure(err) != agentapi.Changed {
		t.Fatal(err)
	}
	var privatePaths []string
	for _, view := range lookup.indexes {
		if view.snapshot != nil {
			privatePaths = append(privatePaths, view.snapshot.dir)
		}
	}
	if err := lookup.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range privatePaths {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if _, err := lookup.Thread(t.Context(), id); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatal(err)
	}
	// A new pass observes the newly committed selection.
	next, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = next.Close() }()
	set, err = next.Thread(t.Context(), id)
	if err != nil || set.Current == nil || set.Current.Path != otherPath {
		t.Fatal(set, err)
	}
}

func TestRolloutLookupRefusesWrongThreadAndOutsideCurrentPaths(t *testing.T) {
	t.Parallel()
	for _, outside := range []bool{false, true} {
		t.Run(map[bool]string{false: "wrong_thread", true: "outside"}[outside], func(t *testing.T) {
			t.Parallel()
			store, cfg, at, root := fixture(t)
			id := writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 1, "sessions")
			otherRoot := root
			if outside {
				otherRoot = t.TempDir()
			}
			other := writeRollout(t, otherRoot, cfg.Archive.Projects[0].Root, at, 2, "sessions")
			path := filepath.Join(otherRoot, "sessions", "rollout-2026-10-01T12-00-00-"+other+".jsonl")
			db := hintDatabase(t, root, true)
			addHint(t, db, id, path, at)
			lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lookup.Close() }()
			if _, err := lookup.Thread(t.Context(), id); agentapi.Failure(err) != agentapi.Unsafe {
				t.Fatal(err)
			}
		})
	}
}

func TestRolloutLookupObservedFactsAreHintsAndRevisionChanges(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	id := writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 1, "sessions")
	path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+id+".jsonl")
	header := sourcefacts.ReadHeader(t.Context(), root, path)
	if header.Identity == nil {
		t.Fatal(header)
	}
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lookup.Close() }()
	identity := *header.Identity
	source := SourceDescriptor{Kind: archive.SourceKindFile, Root: root, Locator: path, StableKey: id}
	lookup.Observe(source, Fingerprint{Size: 1}, &identity)
	initial, err := lookup.Thread(t.Context(), id)
	if err != nil || initial.Complete || initial.Current != nil {
		t.Fatal(initial, err)
	}
	identity.RootID = "caller-mutation"
	if err := lookup.Check(t.Context(), id, initial.Revision); err != nil {
		t.Fatal(err)
	}
	lookup.Observe(source, Fingerprint{Size: 2}, header.Identity)
	if err := lookup.Check(t.Context(), id, initial.Revision); agentapi.Failure(err) != agentapi.Changed {
		t.Fatal(err)
	}
	physical, err := lookup.Rollout(t.Context(), id)
	if err != nil || len(physical) != 1 || physical[0].Path != path {
		t.Fatal(physical, err)
	}
}

func TestRolloutLookupBoundsCurrentIDsAndCancellation(t *testing.T) {
	t.Parallel()
	store, _, _, root := fixture(t)
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lookup.Close() }()
	for i := range maxCurrentThreads {
		id := fmt.Sprintf("00000000-0000-0000-0000-%012x", i)
		if _, err := lookup.Thread(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := lookup.Thread(t.Context(), "11111111-1111-1111-1111-111111111111"); agentapi.Failure(err) != agentapi.Limit {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := lookup.Thread(ctx, "11111111-1111-1111-1111-111111111111"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
