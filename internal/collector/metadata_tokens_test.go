package collector

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// A parser-only bump (0.13.0 to the current DefaultParserVersion) republishes
// nothing but the metadata, from the retained source, even after the native
// log is gone, and the new per-model token fields are in it.
func TestParserBumpRepublishesModelTokensFromRetainedSource(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "archive", "testdata", "codex-model-switch.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "session.jsonl", string(raw))
	reg := registration(t, path)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := &countedPublications{ObjectStore: storagetest.NewMemoryStore()}
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, MachineID: "machine", ParserVersion: "0.13.0", Now: func() time.Time { return now }}
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	old := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	now = now.Add(time.Hour)
	opts.ParserVersion = "" // this build's parser
	remote.keys = nil
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	next := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if next.Parser.Version != archive.DefaultParserVersion || next.Parser.Version == old.Parser.Version {
		t.Fatalf("parser version %q after %q", next.Parser.Version, old.Parser.Version)
	}
	if next.SourceBundle != old.SourceBundle {
		t.Fatalf("source changed: %+v vs %+v", next.SourceBundle, old.SourceBundle)
	}
	metadataKey, _ := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
	// The metadata and its listing entries, never the source object.
	if len(remote.keys) != 3 || remote.keys[2] != metadataKey || slices.Contains(remote.keys, old.SourceBundle.Key) {
		t.Fatalf("a parser bump wrote %v, want only the metadata and its listing", remote.keys)
	}
	var models []string
	for _, entry := range next.ModelTokens {
		models = append(models, entry.Model)
	}
	if !reflect.DeepEqual(models, []string{"gpt-synthetic-a", "gpt-synthetic-b"}) {
		t.Fatalf("model tokens = %+v", next.ModelTokens)
	}
	if next.Counts.ReasoningTokens == nil || *next.Counts.ReasoningTokens != 40 || next.Counts.CacheWriteTokens == nil || *next.Counts.CacheWriteTokens != 57 {
		t.Fatalf("counts = %+v", next.Counts)
	}
}

// Metadata from the previous parser must refresh from retained evidence even
// when the native transcript is gone; import provenance stays attached.
func TestParserUpgradeFrom019RefreshesRetainedImportMetadata(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "archive", "testdata", "codex-model-switch.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "session.jsonl", string(raw))
	reg := registration(t, path)
	reg.Origin = archive.SessionOriginImport
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := &countedPublications{ObjectStore: storagetest.NewMemoryStore()}
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, MachineID: "machine", ParserVersion: "0.19.0", Now: func() time.Time { return now }}
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	old := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	opts.ParserVersion = ""
	remote.keys = nil
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	next := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if next.Parser.Version == old.Parser.Version || next.SourceBundle != old.SourceBundle || next.Origin != archive.SessionOriginImport {
		t.Fatalf("retained metadata did not refresh with provenance: %#v", next)
	}
	if len(remote.keys) != 3 || slices.Contains(remote.keys, old.SourceBundle.Key) {
		t.Fatalf("parser upgrade wrote %v, want metadata and listing only", remote.keys)
	}
}
