package archive

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func retainedHistoryFixture() SourceBundle {
	id := "11111111-1111-4111-8111-111111111111"
	start := uint64(1<<53) + 1
	return SourceBundle{SchemaVersion: HistorySourceSchemaVersion, ArchiveSessionID: "synthetic", NativeSessionID: id, ProjectID: "project", Capture: SourceCapture{Harness: Harness{Name: "codex"}, AdapterName: "codex", CapturedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}, NativeRecords: []map[string]any{{"type": "session_meta", "payload": map[string]any{"id": id}}, {"type": "event_msg", "payload": map[string]any{"type": "task_started"}}}, Ordinals: []uint64{start, start + 2}, History: &SourceHistory{ActiveRolloutID: id, ThreadID: id, Spans: []HistorySpan{{RolloutID: id, ThreadID: id, EndRecord: 2, StartOrdinal: start, EndOrdinal: start + 3}}}}
}
func TestHistorySourcePreservesUnsignedOrdinalPrecision(t *testing.T) {
	t.Parallel()
	b := retainedHistoryFixture()
	c, e := BuildCompressedSource(b)
	if e != nil {
		t.Fatal(e)
	}
	got, e := ReadSourceBundle(bytes.NewReader(c.Bytes), DecodeOptions{})
	if e != nil {
		t.Fatal(e)
	}
	if got.Ordinals[0] != (1<<53)+1 || got.Ordinals[1] != (1<<53)+3 {
		t.Fatalf("rounded unsigned ordinals: %v", got.Ordinals)
	}
	var wire bytes.Buffer
	if e := EncodeSource(&wire, b); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(wire.String(), `"ordinal":9007199254740993`) {
		t.Fatal(wire.String())
	}
}
func TestHistoryManifestRejectsInvalidCoverageAndOwnership(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"gap", "overlap", "wrong_thread", "wrong_active", "ordinal_outside", "ordinal_repeat", "legacy_manifest", "missing_manifest", "text"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			b := retainedHistoryFixture()
			switch kind {
			case "gap":
				b.History.Spans[0].FirstRecord = 1
			case "overlap":
				b.History.Spans = append(b.History.Spans, b.History.Spans[0])
			case "wrong_thread":
				b.History.ThreadID = "22222222-2222-4222-8222-222222222222"
			case "wrong_active":
				b.History.ActiveRolloutID = "22222222-2222-4222-8222-222222222222"
			case "ordinal_outside":
				b.Ordinals[1] = 0
			case "ordinal_repeat":
				b.Ordinals[1] = b.Ordinals[0]
			case "legacy_manifest":
				b.SchemaVersion = SourceSchemaVersion
			case "missing_manifest":
				b.History = nil
			case "text":
				b.NativeText = []TextTranscript{{Content: "unsafe"}}
			}
			if _, e := BuildCompressedSource(b); e == nil {
				t.Fatal("invalid history accepted")
			}
		})
	}
}
func TestHistoryMetadataReferencesStayWithinTheirSession(t *testing.T) {
	t.Parallel()
	b := retainedHistoryFixture()
	c, e := BuildCompressedSource(b)
	if e != nil {
		t.Fatal(e)
	}
	key, e := SourceObjectKey(b, c.SHA256)
	if e != nil {
		t.Fatal(e)
	}
	m := Metadata{SchemaVersion: HistoryMetadataSchemaVersion, SessionID: b.ArchiveSessionID, NativeSessionID: b.NativeSessionID, ProjectID: b.ProjectID, Harness: b.Capture.Harness, CapturedAt: b.Capture.CapturedAt, SourceBundle: SourceReference{Key: key, SHA256: c.SHA256, CompressedBytes: len(c.Bytes)}, History: &RevisionHistory{CurrentRevision: b.History.ActiveRolloutID}}
	if refs, e := m.SourceReferences(); e != nil || len(refs) != 1 {
		t.Fatalf("valid refs %v %v", refs, e)
	}
	m.History.Preserved = []RevisionReference{{RevisionID: "22222222-2222-4222-8222-222222222222", CapturedAt: m.CapturedAt, Source: m.SourceBundle}}
	if e := m.ValidateSourceReference(); e == nil {
		t.Fatal("duplicate source accepted")
	}
	m.History.Preserved[0].Source.Key = strings.Replace(key, "synthetic", "other", 1)
	if e := m.ValidateSourceReference(); e == nil {
		t.Fatal("foreign source accepted")
	}
	m.History = nil
	if e := m.ValidateSourceReference(); e == nil {
		t.Fatal("schema2 without history accepted")
	}
}
func FuzzHistoryManifest(f *testing.F) {
	raw, _ := json.Marshal(retainedHistoryFixture().History)
	f.Add(raw)
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 65536 {
			return
		}
		b := retainedHistoryFixture()
		var h SourceHistory
		if json.Unmarshal(raw, &h) != nil {
			return
		}
		b.History = &h
		if b.ValidateHistory() != nil {
			return
		}
		compressed, e := BuildCompressedSource(b)
		if e != nil {
			t.Fatal(e)
		}
		if _, e := ReadSourceBundle(bytes.NewReader(compressed.Bytes), DecodeOptions{}); e != nil {
			t.Fatal(e)
		}
	})
}
