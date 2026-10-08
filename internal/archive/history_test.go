package archive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"strings"
	"testing"
	"time"
)

type manifestScenario string

const (
	manifestScenarioGap             manifestScenario = "gap"
	manifestScenarioOverlap         manifestScenario = "overlap"
	manifestScenarioWrongThread     manifestScenario = "wrong_thread"
	manifestScenarioWrongActive     manifestScenario = "wrong_active"
	manifestScenarioOrdinalOutside  manifestScenario = "ordinal_outside"
	manifestScenarioOrdinalRepeat   manifestScenario = "ordinal_repeat"
	manifestScenarioLegacyManifest  manifestScenario = "legacy_manifest"
	manifestScenarioMissingManifest manifestScenario = "missing_manifest"
	manifestScenarioText            manifestScenario = "text"
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
	for _, kind := range []manifestScenario{manifestScenarioGap, manifestScenarioOverlap, manifestScenarioWrongThread, manifestScenarioWrongActive, manifestScenarioOrdinalOutside, manifestScenarioOrdinalRepeat, manifestScenarioLegacyManifest, manifestScenarioMissingManifest, manifestScenarioText} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			b := retainedHistoryFixture()
			switch kind {
			case manifestScenarioGap:
				b.History.Spans[0].FirstRecord = 1
			case manifestScenarioOverlap:
				b.History.Spans = append(b.History.Spans, b.History.Spans[0])
			case manifestScenarioWrongThread:
				b.History.ThreadID = "22222222-2222-4222-8222-222222222222"
			case manifestScenarioWrongActive:
				b.History.ActiveRolloutID = "22222222-2222-4222-8222-222222222222"
			case manifestScenarioOrdinalOutside:
				b.Ordinals[1] = 0
			case manifestScenarioOrdinalRepeat:
				b.Ordinals[1] = b.Ordinals[0]
			case manifestScenarioLegacyManifest:
				b.SchemaVersion = SourceSchemaVersion
			case manifestScenarioMissingManifest:
				b.History = nil
			case manifestScenarioText:
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

func TestHistoryOutputMatchesPublishedSchemas(t *testing.T) {
	t.Parallel()
	sourceSchema := compileSchema(t, "source-bundle.schema.json")
	metadataSchema := compileSchema(t, "metadata.schema.json")
	b := retainedHistoryFixture()
	b.Capture.AdapterVersion = "0.15.0"
	b.Capture.FilterVersion = FilterVersion
	b.Capture.SourceFormat = "codex-jsonl"
	var wire bytes.Buffer
	if e := EncodeSource(&wire, b); e != nil {
		t.Fatal(e)
	}
	for line := range bytes.SplitSeq(bytes.TrimSpace(wire.Bytes()), []byte{'\n'}) {
		value, e := jsonschema.UnmarshalJSON(bytes.NewReader(line))
		if e != nil {
			t.Fatal(e)
		}
		if e := sourceSchema.Validate(value); e != nil {
			t.Fatal(e)
		}
	}
	compressed, e := BuildCompressedSource(b)
	if e != nil {
		t.Fatal(e)
	}
	key, e := SourceObjectKey(b, compressed.SHA256)
	if e != nil {
		t.Fatal(e)
	}
	metadata, e := BuildMetadataWithAnalysis(b, Analysis{}, nil, "synthetic-machine", b.Capture.CapturedAt, b.Capture.CapturedAt, SourceReference{Key: key, SHA256: compressed.SHA256, CompressedBytes: len(compressed.Bytes)}, ParserInfo{Name: "codex", Version: "0.22.0"})
	if e != nil {
		t.Fatal(e)
	}
	previous := b
	previous.Capture.CapturedAt = previous.Capture.CapturedAt.Add(-time.Hour)
	history := *previous.History
	history.ActiveRolloutID = "22222222-2222-4222-8222-222222222222"
	history.Spans = append([]HistorySpan(nil), history.Spans...)
	history.Spans[len(history.Spans)-1].RolloutID = history.ActiveRolloutID
	previous.History = &history
	priorCompressed, e := BuildCompressedSource(previous)
	if e != nil {
		t.Fatal(e)
	}
	priorKey, e := SourceObjectKey(previous, priorCompressed.SHA256)
	if e != nil {
		t.Fatal(e)
	}
	metadata.History.Preserved = []RevisionReference{{RevisionID: "22222222-2222-4222-8222-222222222222", CapturedAt: previous.Capture.CapturedAt, SourceSchemaVersion: 3, FilterVersion: previous.Capture.FilterVersion, Source: SourceReference{Key: priorKey, SHA256: priorCompressed.SHA256, CompressedBytes: len(priorCompressed.Bytes)}}}
	encoded, e := json.Marshal(metadata)
	if e != nil {
		t.Fatal(e)
	}
	value, e := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if e != nil {
		t.Fatal(e)
	}
	if e := metadataSchema.Validate(value); e != nil {
		t.Fatal(e)
	}
}

func TestSourceSetDigestIncludesActiveCaptureAndRevisionProvenance(t *testing.T) {
	t.Parallel()
	b := retainedHistoryFixture()
	packed, err := BuildCompressedSource(b)
	if err != nil {
		t.Fatal(err)
	}
	key, err := SourceObjectKey(b, packed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	ref := SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
	m := Metadata{SchemaVersion: HistoryMetadataSchemaVersion, SessionID: b.ArchiveSessionID, NativeSessionID: b.NativeSessionID, ProjectID: b.ProjectID, Harness: b.Capture.Harness, CapturedAt: b.Capture.CapturedAt, SourceBundle: ref, History: &RevisionHistory{CurrentRevision: b.History.ActiveRolloutID}}
	first, err := m.SourceSetDigest()
	if err != nil {
		t.Fatal(err)
	}
	m.CapturedAt = m.CapturedAt.Add(time.Second)
	changed, err := m.SourceSetDigest()
	if err != nil || first == changed {
		t.Fatal("active capture omitted", err)
	}
	b.Capture.CapturedAt = b.Capture.CapturedAt.Add(-time.Hour)
	packed, err = BuildCompressedSource(b)
	if err != nil {
		t.Fatal(err)
	}
	key, err = SourceObjectKey(b, packed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	m.History.Preserved = []RevisionReference{{RevisionID: "22222222-2222-4222-8222-222222222222", CapturedAt: b.Capture.CapturedAt, Source: SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}}}
	first, err = m.SourceSetDigest()
	if err != nil {
		t.Fatal(err)
	}
	m.History.Preserved[0].SourceSchemaVersion = 3
	m.History.Preserved[0].FilterVersion = "14"
	changed, err = m.SourceSetDigest()
	if err != nil || first == changed {
		t.Fatal("revision provenance omitted", err)
	}
}

// Preserved source references across revisions are distinct from one graph's nodes.
func TestHistorySourceUnionKeepsActivePlus64Preserved(t *testing.T) {
	b := retainedHistoryFixture()
	makeRef := func(i int) SourceReference {
		hash := fmt.Sprintf("%064x", i+1)
		key, err := SourceObjectKey(b, hash)
		if err != nil {
			t.Fatal(err)
		}
		return SourceReference{Key: key, SHA256: hash, CompressedBytes: 1}
	}
	m := Metadata{SchemaVersion: HistoryMetadataSchemaVersion, SessionID: b.ArchiveSessionID, NativeSessionID: b.NativeSessionID, ProjectID: b.ProjectID, Harness: b.Capture.Harness, CapturedAt: b.Capture.CapturedAt, SourceBundle: makeRef(0), History: &RevisionHistory{CurrentRevision: b.History.ActiveRolloutID}}
	for i := range 64 {
		m.History.Preserved = append(m.History.Preserved, RevisionReference{RevisionID: fmt.Sprintf("%08x-2222-4222-8222-222222222222", i+1), CapturedAt: m.CapturedAt, Source: makeRef(i + 1)})
	}
	refs, err := m.SourceReferences()
	if err != nil || len(refs) != 65 {
		t.Fatal("active plus64 preserved rejected", len(refs), err)
	}
	m.History.Preserved = append(m.History.Preserved, RevisionReference{RevisionID: "ffffffff-2222-4222-8222-222222222222", CapturedAt: m.CapturedAt, Source: makeRef(65)})
	if _, err := m.SourceReferences(); err == nil {
		t.Fatal("65 preserved refs accepted")
	}
}
