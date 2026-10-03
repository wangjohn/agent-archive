package collector

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

const moreTranscript = `
{"type":"response_item","id":"m2","payload":{"type":"message","role":"assistant","content":"more"}}`

// A key derived from the project root stays in the sidecar when a later
// lookup fails (git briefly unavailable, the checkout moved), and a lookup
// that succeeds with another answer (the remote really changed) replaces it.
func TestDerivedRepoKeyIsStickyAcrossPublications(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript)
	reg := registration(t, path)
	remote := storagetest.NewMemoryStore()
	now := reg.RegisteredAt.Add(time.Hour)
	git := &countingLookup{keys: map[string]string{"/p": widgetKey}}
	opts := Options{Sources: testSources, MachineID: "machine", RepoKey: git.lookup, Now: func() time.Time { return now }}
	if got := publishOnce(t, local, remote, reg, &opts); got.RepoKey != widgetKey {
		t.Fatalf("first publication repo_key = %q, want %q", got.RepoKey, widgetKey)
	}

	content := codexTranscript
	grow := func(extra string) {
		t.Helper()
		content += extra
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Hour)
		result, err := Run(context.Background(), local, remote, opts)
		if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
			t.Fatalf("%#v %v", result, err)
		}
	}

	// git fails this time: the published key stays.
	git.keys = map[string]string{}
	grow(moreTranscript)
	if got := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID).RepoKey; got != widgetKey {
		t.Errorf("after a failed lookup repo_key = %q, want the earlier %q", got, widgetKey)
	}

	// The remote really changed: the new key replaces it.
	git.keys = map[string]string{"/p": gadgetKey}
	grow("\n" + `{"type":"response_item","id":"m3","payload":{"type":"message","role":"assistant","content":"again"}}`)
	if got := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID).RepoKey; got != gadgetKey {
		t.Errorf("after the remote changed repo_key = %q, want %q", got, gadgetKey)
	}
}

func TestDerivedRepoKeyIsStickyAcrossAParserRefresh(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	remote := storagetest.NewMemoryStore()
	now := reg.RegisteredAt.Add(time.Hour)
	git := &countingLookup{keys: map[string]string{"/p": widgetKey}}
	opts := Options{Sources: testSources, MachineID: "machine", ParserVersion: "one", RepoKey: git.lookup, Now: func() time.Time { return now }}
	if got := publishOnce(t, local, remote, reg, &opts); got.RepoKey != widgetKey {
		t.Fatalf("setup: repo_key = %q", got.RepoKey)
	}

	// The next parser finds no repository (the checkout is gone): the key
	// already published is kept.
	now = now.Add(time.Hour)
	git.keys = map[string]string{}
	opts.ParserVersion = "two"
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if after.Parser.Version != "two" || after.RepoKey != widgetKey {
		t.Errorf("refreshed sidecar = parser %q repo_key %q, want two and %q", after.Parser.Version, after.RepoKey, widgetKey)
	}

	// And a refresh that does find a different remote takes it.
	now = now.Add(time.Hour)
	git.keys = map[string]string{"/p": gadgetKey}
	opts.ParserVersion = "three"
	if _, err := Run(context.Background(), local, remote, opts); err != nil {
		t.Fatal(err)
	}
	if got := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID).RepoKey; got != gadgetKey {
		t.Errorf("after a refresh that found a new remote repo_key = %q, want %q", got, gadgetKey)
	}
}

// A key on the registration is what the hook saw at the start of the session:
// it is used as recorded, not re-derived, whatever git says later.
func TestRegisteredRepoKeyIsNotReDerived(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	reg.RepoKey = widgetKey
	remote := storagetest.NewMemoryStore()
	now := reg.RegisteredAt.Add(time.Hour)
	git := &countingLookup{keys: map[string]string{"/p": gadgetKey}}
	opts := Options{Sources: testSources, MachineID: "machine", ParserVersion: "one", RepoKey: git.lookup, Now: func() time.Time { return now }}
	publishOnce(t, local, remote, reg, &opts)
	now = now.Add(time.Hour)
	opts.ParserVersion = "two"
	if _, err := Run(context.Background(), local, remote, opts); err != nil {
		t.Fatal(err)
	}
	if got := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID).RepoKey; got != widgetKey || git.total() != 0 {
		t.Errorf("repo_key = %q after %d lookups, want the registered %q and none", got, git.total(), widgetKey)
	}
}
