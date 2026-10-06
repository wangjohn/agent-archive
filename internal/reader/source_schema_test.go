package reader

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// A bundle written as schema 1 (one gzip-compressed JSON document) passes the
// size and hash checks, since those cover whatever bytes were stored, and is
// then refused with an error that names its schema version.
func TestLoadSourceRefusesSchemaOneBundles(t *testing.T) {
	metadata, bundle, store := fixture(t)
	bundle.SchemaVersion = 1
	document, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(document); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	legacyKey := strings.TrimSuffix(metadata.SourceBundle.Key, ".jsonl.gz") + ".json.gz"
	if err := store.Put(context.Background(), legacyKey, compressed.Bytes()); err != nil {
		t.Fatal(err)
	}
	metadata.SourceBundle = archive.SourceReference{Key: legacyKey, SHA256: storage.SHA256Hex(compressed.Bytes()), CompressedBytes: compressed.Len()}
	_, err = LoadSource(context.Background(), store, metadata, Limits{})
	if err == nil || !strings.Contains(err.Error(), "unsupported source schema version 1") {
		t.Fatalf("schema 1 bundle error = %v", err)
	}
}

// A current bundle loads through the streaming decoder, and the uncompressed
// read limit still applies to it.
func TestLoadSourceStreamsAndKeepsTheUncompressedLimit(t *testing.T) {
	metadata, want, store := fixture(t)
	got, err := LoadSource(context.Background(), store, metadata, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("loaded bundle differs:\n%s\n%s", gotJSON, wantJSON)
	}
	if !strings.HasSuffix(metadata.SourceBundle.Key, ".jsonl.gz") {
		t.Fatalf("source key = %q", metadata.SourceBundle.Key)
	}
	if _, err := LoadSource(context.Background(), store, metadata, Limits{MaxUncompressedBytes: 32}); err == nil || !strings.Contains(err.Error(), "uncompressed read limit") {
		t.Fatalf("uncompressed limit error = %v", err)
	}
}

func TestLoadHistorySourceValidatesActiveRevisionAndReferences(t *testing.T) {
	t.Parallel()
	metadata, bundle, store := fixture(t)
	const thread = "11111111-1111-4111-8111-111111111111"
	const revision = "22222222-2222-4222-8222-222222222222"
	bundle.SchemaVersion = archive.HistorySourceSchemaVersion
	bundle.NativeSessionID = thread
	bundle.Ordinals = []uint64{0, 1}
	bundle.History = &archive.SourceHistory{ThreadID: thread, ActiveRolloutID: revision, Spans: []archive.HistorySpan{{ThreadID: thread, RolloutID: revision, EndRecord: 2, EndOrdinal: 2}}}
	packed, err := archive.BuildCompressedSource(bundle)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.SourceObjectKey(bundle, packed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	metadata.SchemaVersion = archive.HistoryMetadataSchemaVersion
	metadata.NativeSessionID = thread
	metadata.History = &archive.RevisionHistory{CurrentRevision: revision}
	metadata.SourceBundle = archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
	if err := store.Put(t.Context(), key, packed.Bytes); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), "sessions/codex/session-1/metadata.json", raw); err != nil {
		t.Fatal(err)
	}
	selected, err := ReadMetadata(t.Context(), store, "sessions/codex/session-1/metadata.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadSource(t.Context(), store, selected, Limits{})
	if err != nil || got.History == nil || got.History.ActiveRolloutID != revision {
		t.Fatalf("history readback: %+v %v", got.History, err)
	}
	selected.History.CurrentRevision = thread
	if _, err := LoadSource(t.Context(), store, selected, Limits{}); err == nil {
		t.Fatal("wrong active revision accepted")
	}
	selected.History.CurrentRevision = revision
	selected.History.Preserved = []archive.RevisionReference{{RevisionID: thread, CapturedAt: selected.CapturedAt, Source: archive.SourceReference{Key: "sessions/codex/foreign/source." + packed.SHA256 + ".jsonl.gz", SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}}}
	if _, err := LoadSource(t.Context(), store, selected, Limits{}); err == nil {
		t.Fatal("foreign preserved source reference accepted")
	}
}
