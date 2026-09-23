package collector

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// A collector upgraded to source schema 2 still has schema-1 bundles in its
// local published cache. They must not fail validation on every pass: the
// in-memory bundle's shape is unchanged, so a cached schema-1 bundle is used
// as schema 2, the next change publishes a .jsonl.gz source, and the pass
// records no error.
func TestCachedSchemaOneBundleDoesNotWedgePublication(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	store := newTestStore(t)
	if err := store.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	remote := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), store, remote, Options{MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}

	// Rewrite the cache as a schema-1 build left it.
	var state map[string]any
	if err := local.Read(store.publishedPath("session-1"), &state); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"bundle"} {
		if bundle, ok := state[key].(map[string]any); ok {
			bundle["schema_version"] = 1
		}
	}
	if last, ok := state["last_published"].(map[string]any); ok {
		if bundle, ok := last["bundle"].(map[string]any); ok {
			bundle["schema_version"] = 1
		}
	}
	if err := local.Write(store.publishedPath("session-1"), state); err != nil {
		t.Fatal(err)
	}
	cached, _, _, _, err := store.LoadPublished("session-1")
	if err != nil || cached.SchemaVersion != archive.SourceSchemaVersion {
		t.Fatalf("cached schema-1 bundle was not read as schema %d: %d err=%v", archive.SourceSchemaVersion, cached.SchemaVersion, err)
	}

	writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"m2","payload":{"type":"message","role":"user","content":"more"}}`)
	t1 := t0.Add(time.Hour)
	for pass := 0; pass < 2; pass++ {
		result, err := Run(context.Background(), store, remote, Options{MachineID: "m", Now: func() time.Time { return t1.Add(time.Duration(pass) * time.Minute) }})
		if err != nil || len(result.Errors) != 0 {
			t.Fatalf("pass %d: result=%#v err=%v", pass, result, err)
		}
	}
	metadata := fetchMetadata(t, remote, "codex", "session-1")
	if !strings.HasSuffix(metadata.SourceBundle.Key, ".jsonl.gz") {
		t.Fatalf("source key = %q", metadata.SourceBundle.Key)
	}
	bundle := fetchBundle(t, remote, metadata)
	if bundle.SchemaVersion != archive.SourceSchemaVersion || len(bundle.NativeRecords) != 3 {
		t.Fatalf("published bundle = schema %d with %d records", bundle.SchemaVersion, len(bundle.NativeRecords))
	}
}
