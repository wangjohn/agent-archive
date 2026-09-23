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
