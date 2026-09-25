package archive

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fixtureBundles builds a source bundle from every JSONL fixture, with
// supplemental evidence and a linked session so every line kind appears, plus
// one Cursor text bundle for native_text lines and one Cursor database chat.
func fixtureBundles(t *testing.T) map[string]SourceBundle {
	t.Helper()
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	captured := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	evidence := []SupplementalEvidence{
		{Kind: EvidenceKindSkillInventory, ObservedAt: captured, Provenance: "filesystem:codex", Payload: map[string]any{"coverage": "installed_only", "scope": "user_agents", "skills": []any{map[string]any{"name": "review", "sha256": strings.Repeat("a", 64)}}}},
		{Kind: EvidenceKindExplicitFeedback, ObservedAt: captured, Provenance: "user:feedback", Payload: map[string]any{"text": "looks right"}},
		{Kind: EvidenceKindLinkedSession, ObservedAt: captured, Provenance: "hook:subagent-link", Payload: map[string]any{"archive_session_id": "child-1", "relationship": "subagent", "status": "published"}},
	}
	// Top-level fixtures are named <harness>-<shape>.jsonl; the handoff
	// transcripts live in testdata/handoff/<harness>.jsonl.
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		names = append(names, entry.Name())
	}
	for _, harness := range []string{"claude", "codex", "cursor"} {
		names = append(names, filepath.Join("handoff", harness+".jsonl"))
	}
	out := map[string]SourceBundle{}
	for _, name := range names {
		harness := strings.SplitN(filepath.Base(name), "-", 2)[0]
		harness = strings.TrimSuffix(harness, ".jsonl")
		adapter, err := NewAdapter(harness)
		if err != nil || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		filtered, err := adapter.FilterJSONL(bytes.NewReader(fixture(t, name)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		reg := registration()
		reg.Harness = Harness{Name: adapter.Name()}
		reg.ParentSessionID = "parent-1"
		bundle, err := NewSourceBundle(reg, adapter, filtered, captured, evidence)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out[name] = bundle
	}
	text, err := (CursorAdapter{}).FilterText(strings.NewReader("user: hello\nassistant: hi there\n"), captured)
	if err != nil {
		t.Fatal(err)
	}
	reg := registration()
	reg.Harness = Harness{Name: "cursor"}
	bundle, err := NewSourceBundle(reg, CursorAdapter{}, text, captured, evidence)
	if err != nil {
		t.Fatal(err)
	}
	out["cursor-text"] = bundle
	composer, err := (CursorAdapter{}).FilterComposer(loadComposerFixture(t, "chat.json"))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err = NewSourceBundle(reg, CursorAdapter{}, composer, captured, evidence)
	if err != nil {
		t.Fatal(err)
	}
	out["cursor-composer"] = bundle
	if len(out) < 10 {
		t.Fatalf("only %d fixture bundles were built", len(out))
	}
	return out
}

func canonicalJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// Every fixture survives build -> compress -> streaming decode unchanged, and
// re-encoding the decoded bundle reproduces the exact compressed bytes.
func TestSourceBundleJSONLRoundTripsEveryFixture(t *testing.T) {
	for name, bundle := range fixtureBundles(t) {
		compressed, err := BuildCompressedSource(bundle)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		decoded, err := ReadSourceBundle(bytes.NewReader(compressed.Bytes), DecodeOptions{})
		if err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		if canonicalJSON(t, decoded) != canonicalJSON(t, bundle) {
			t.Fatalf("%s: decoded bundle differs\nwant %s\ngot  %s", name, canonicalJSON(t, bundle), canonicalJSON(t, decoded))
		}
		again, err := BuildCompressedSource(decoded)
		if err != nil {
			t.Fatal(err)
		}
		if again.SHA256 != compressed.SHA256 || !bytes.Equal(again.Bytes, compressed.Bytes) {
			t.Fatalf("%s: re-encoding the decoded bundle changed its bytes", name)
		}
	}
}

func TestSourceBundleJSONLBuildsAreByteIdentical(t *testing.T) {
	for name, bundle := range fixtureBundles(t) {
		first, err := BuildCompressedSource(bundle)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 3 {
			next, err := BuildCompressedSource(bundle)
			if err != nil {
				t.Fatal(err)
			}
			if next.SHA256 != first.SHA256 || !bytes.Equal(next.Bytes, first.Bytes) {
				t.Fatalf("%s: build %d differs", name, i+2)
			}
		}
		key, err := SourceObjectKey(bundle, first.SHA256)
		if err != nil || !strings.HasSuffix(key, "/source."+first.SHA256+".jsonl.gz") {
			t.Fatalf("%s: key %q err=%v", name, key, err)
		}
	}
}

// The decompressed form is one object per line, header first, with the kinds
// in their fixed order.
func TestSourceBundleJSONLLineLayout(t *testing.T) {
	bundle := fixtureBundles(t)["claude-tool-evidence.jsonl"]
	var plain bytes.Buffer
	if err := EncodeSource(&plain, bundle); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(plain.String(), "\n"), "\n")
	wantLines := 1 + len(bundle.NativeRecords) + len(bundle.NativeText) + len(bundle.SupplementalEvidence)
	if len(lines) != wantLines {
		t.Fatalf("%d lines, want %d", len(lines), wantLines)
	}
	var header map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil || header["kind"] != "header" || header["schema_version"] != float64(2) {
		t.Fatalf("header line = %s", lines[0])
	}
	counts, _ := header["counts"].(map[string]any)
	if counts["native_records"] != float64(len(bundle.NativeRecords)) || counts["supplemental_evidence"] != float64(len(bundle.SupplementalEvidence)) {
		t.Fatalf("header counts = %#v", counts)
	}
	if !strings.HasPrefix(lines[1], `{"kind":"native_record","record":`) {
		t.Fatalf("line 2 = %.60s", lines[1])
	}
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, `{"kind":"supplemental_evidence","evidence":{"kind":`) {
		t.Fatalf("last line = %.80s", last)
	}
}

func gzipLines(t *testing.T, lines ...string) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	for _, line := range lines {
		if _, err := writer.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func headerLine(records, evidence int) string {
	return fmt.Sprintf(`{"kind":"header","schema_version":2,"archive_session_id":"a","native_session_id":"n","project_id":"p","capture":{"harness":{"name":"codex"},"adapter_name":"codex","adapter_version":"0.4.0","source_format":"codex-jsonl","boundary":{"retained_records":1,"retained_bytes":1},"filter_version":"4","captured_at":"2026-09-23T10:00:00Z"},"counts":{"native_records":%d,"native_text":0,"supplemental_evidence":%d}}`, records, evidence)
}

const recordLine = `{"kind":"native_record","record":{"type":"response_item"}}`

const evidenceLineJSON = `{"kind":"supplemental_evidence","evidence":{"kind":"explicit_feedback","observed_at":"2026-09-23T10:00:00Z","provenance":"user","payload":{"text":"ok"}}}`

func TestSourceBundleJSONLRejectsMalformedStreams(t *testing.T) {
	v1, err := json.Marshal(SourceBundle{SchemaVersion: 1, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p", NativeRecords: []map[string]any{{"type": "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	valid := gzipLines(t, headerLine(1, 0), recordLine)
	cases := []struct {
		name    string
		input   []byte
		options DecodeOptions
		want    string
	}{
		{"header not first", gzipLines(t, recordLine, headerLine(1, 0)), DecodeOptions{}, "header must come first"},
		{"second header", gzipLines(t, headerLine(1, 0), headerLine(1, 0), recordLine), DecodeOptions{}, "second header"},
		{"fewer lines than counted", gzipLines(t, headerLine(2, 0), recordLine), DecodeOptions{}, "do not match"},
		{"more lines than counted", gzipLines(t, headerLine(1, 0), recordLine, recordLine), DecodeOptions{}, "more native records"},
		{"kinds out of order", gzipLines(t, headerLine(1, 1), evidenceLineJSON, recordLine), DecodeOptions{}, "out of order"},
		{"unknown kind", gzipLines(t, headerLine(0, 0), `{"kind":"mystery"}`), DecodeOptions{}, "unknown kind"},
		{"invalid JSON line", gzipLines(t, headerLine(1, 0), `{"kind":"native_record",`), DecodeOptions{}, "not valid JSON"},
		{"truncated gzip", valid[:len(valid)-12], DecodeOptions{}, "truncated"},
		{"not gzip", []byte("plain text"), DecodeOptions{}, "open source gzip"},
		{"empty stream", gzipLines(t), DecodeOptions{}, "no header"},
		{"over-long line", gzipLines(t, headerLine(1, 0), `{"kind":"native_record","record":{"text":"`+strings.Repeat("x", 4096)+`"}}`), DecodeOptions{MaxLineBytes: 2048}, "exceeds the 2048 byte line limit"},
		{"schema 1 document", gzipLines(t, string(v1)), DecodeOptions{}, "unsupported source schema version 1"},
		{"schema 1 document as written (no newline)", gzipBytes(t, v1), DecodeOptions{}, "unsupported source schema version 1"},
		{"final line without newline", gzipBytes(t, []byte(headerLine(1, 0)+"\n"+recordLine)), DecodeOptions{}, "truncated"},
		{"future schema", gzipLines(t, strings.Replace(headerLine(0, 0), `"schema_version":2`, `"schema_version":3`, 1)), DecodeOptions{}, "unsupported source schema version 3"},
		{"uncompressed limit", valid, DecodeOptions{MaxUncompressedBytes: 64}, ErrSourceTooLarge.Error()},
		{"trailing bytes after the gzip stream", append(append([]byte{}, valid...), "trailing garbage bytes"...), DecodeOptions{}, "trailing bytes after its gzip stream"},
		// Fewer trailing bytes than a gzip header is indistinguishable from a
		// cut-off second gzip member, so it is reported as truncation.
		{"trailing bytes shorter than a gzip header", append(append([]byte{}, valid...), "tail"...), DecodeOptions{}, "source is truncated"},
		{"line 1 is a JSON number", gzipLines(t, `42`), DecodeOptions{}, "line 1 is not a JSON object"},
		{"line 1 is a JSON array", gzipLines(t, `[]`, recordLine), DecodeOptions{}, "line 1 is not a JSON object"},
		{"line 2 is a JSON string", gzipLines(t, headerLine(1, 0), `"native_record"`), DecodeOptions{}, "line 2 is not a JSON object"},
		{"line 2 is null", gzipLines(t, headerLine(1, 0), `null`), DecodeOptions{}, "line 2 is not a JSON object"},
		{"blank line", gzipLines(t, headerLine(1, 0), ``, recordLine), DecodeOptions{}, "line 2 is not a JSON object"},
	}
	for _, c := range cases {
		_, err := ReadSourceBundle(bytes.NewReader(c.input), c.options)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", c.name, err, c.want)
		}
	}
	if _, err := ReadSourceBundle(bytes.NewReader(valid), DecodeOptions{}); err != nil {
		t.Fatalf("the valid control stream failed: %v", err)
	}
}

// The line cap is inclusive on the decode side exactly as it is on the encode
// side: a line of exactly the cap decodes, one byte more does not.
func TestDecodeSourceLineCapIsExact(t *testing.T) {
	const limit = 4096
	lineOf := func(n int) string {
		prefix, suffix := `{"kind":"native_record","record":{"text":"`, `"}}`
		return prefix + strings.Repeat("x", n-len(prefix)-len(suffix)) + suffix
	}
	if got := len(lineOf(limit)); got != limit {
		t.Fatalf("test line is %d bytes, want %d", got, limit)
	}
	options := DecodeOptions{MaxLineBytes: limit}
	if _, err := ReadSourceBundle(bytes.NewReader(gzipLines(t, headerLine(1, 0), lineOf(limit))), options); err != nil {
		t.Fatalf("a line of exactly %d bytes was refused: %v", limit, err)
	}
	_, err := ReadSourceBundle(bytes.NewReader(gzipLines(t, headerLine(1, 0), lineOf(limit+1))), options)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("line 2 exceeds the %d byte line limit", limit)) {
		t.Fatalf("a line of %d bytes: err = %v", limit+1, err)
	}
	// An over-long line that is also the last, unterminated one is refused
	// as well: as over-long once it overflows the buffer, or as truncated
	// when EOF arrives first (one byte over the cap fits the buffer).
	for _, n := range []int{limit + 1, 2 * limit} {
		_, err = ReadSourceBundle(bytes.NewReader(gzipBytes(t, []byte(headerLine(1, 0)+"\n"+lineOf(n)))), options)
		if err == nil || (!strings.Contains(err.Error(), "line limit") && !strings.Contains(err.Error(), "truncated")) {
			t.Fatalf("unterminated %d byte line: err = %v", n, err)
		}
	}
}

// Hitting the total cap in the middle of a line reports the cap, not a
// truncated stream.
func TestDecodeSourceReportsTotalCapHitMidLine(t *testing.T) {
	stream := gzipBytes(t, []byte(headerLine(1, 0)+"\n"+recordLine))
	_, err := ReadSourceBundle(bytes.NewReader(stream), DecodeOptions{MaxUncompressedBytes: 64})
	if !errors.Is(err, ErrSourceTooLarge) || err.Error() != ErrSourceTooLarge.Error() {
		t.Fatalf("err = %v, want exactly %v", err, ErrSourceTooLarge)
	}
}

// The views built on a bundle are identical whether the bundle came from
// memory or through the compressed JSONL decoder: the normalized view for
// every fixture, and the handoff document for each harness's golden file.
func TestSourceBundleJSONLPreservesNormalizedAndHandoffViews(t *testing.T) {
	throughDecoder := func(t *testing.T, bundle SourceBundle) SourceBundle {
		t.Helper()
		compressed, err := BuildCompressedSource(bundle)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := ReadSourceBundle(bytes.NewReader(compressed.Bytes), DecodeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	for name, bundle := range fixtureBundles(t) {
		want, err := ParseNormalized(bundle)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := ParseNormalized(throughDecoder(t, bundle))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if canonicalJSON(t, got) != canonicalJSON(t, want) {
			t.Fatalf("%s: normalized view differs after decoding", name)
		}
	}
	for _, harness := range []string{"claude", "codex", "cursor"} {
		h, err := BuildHandoff(throughDecoder(t, handoffBundle(t, harness)), nil, HandoffOptions{Source: "local"})
		if err != nil {
			t.Fatal(err)
		}
		got := RenderHandoffMarkdown(h, HandoffRenderOptions{Preamble: true})
		want, err := os.ReadFile(filepath.Join("testdata", "handoff", harness+".md"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("handoff for %s differs from its golden file after decoding", harness)
		}
	}
}

// A callback error stops the decode and is returned as is.
func TestDecodeSourceStopsOnCallbackError(t *testing.T) {
	stop := errors.New("stop here")
	lines := 0
	err := DecodeSource(bytes.NewReader(gzipLines(t, headerLine(2, 0), recordLine, recordLine)), DecodeOptions{}, func(SourceLine) error {
		lines++
		if lines == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || lines != 2 {
		t.Fatalf("err=%v lines=%d", err, lines)
	}
}

// Streaming keeps memory bounded by one line, not by the bundle: decoding a
// bundle of many records while discarding each one never holds anything near
// the whole decompressed document. The measured peak is logged for the
// implementation ledger.
func TestDecodeSourceStreamsWithBoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates a large synthetic bundle")
	}
	const records = 12000
	// The bundle is built in largeCompressedSource and dropped when it
	// returns, so only the compressed bytes stay live.
	compressed, plain := largeCompressedSource(t, records)
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	baseline := stats.HeapAlloc
	peak := baseline
	decoded := 0
	err := DecodeSource(bytes.NewReader(compressed.Bytes), DecodeOptions{}, func(line SourceLine) error {
		if line.Kind == SourceLineNativeRecord {
			decoded++
			if decoded%250 == 0 {
				runtime.ReadMemStats(&stats)
				if stats.HeapAlloc > peak {
					peak = stats.HeapAlloc
				}
			}
		}
		return nil
	})
	if err != nil || decoded != records {
		t.Fatalf("decoded=%d err=%v", decoded, err)
	}
	growth := int64(peak) - int64(baseline)
	t.Logf("uncompressed %d bytes, compressed %d bytes, peak heap growth during decode %d bytes (%.1f%% of uncompressed)",
		plain.n, len(compressed.Bytes), growth, 100*float64(growth)/float64(plain.n))
	if plain.n < 30<<20 {
		t.Fatalf("synthetic bundle is only %d bytes; the test needs a large one", plain.n)
	}
	if growth > plain.n/4 {
		t.Fatalf("decode heap grew by %d bytes for a %d byte bundle: not streaming", growth, plain.n)
	}
}

// largeCompressedSource builds a bundle of that many ~3 KB native records
// and returns it compressed, with the size of its uncompressed encoding.
func largeCompressedSource(t *testing.T, records int) (CompressedSource, countingWriter) {
	t.Helper()
	filler := strings.Repeat("synthetic retained tool output line. ", 80) // ~3 KB per record
	nativeRecords := make([]map[string]any, 0, records)
	for i := range records {
		nativeRecords = append(nativeRecords, map[string]any{"type": "response_item", "id": fmt.Sprintf("item-%05d", i), "payload": map[string]any{"type": "function_call_output", "output": filler}})
	}
	bundle := SourceBundle{
		SchemaVersion: SourceSchemaVersion, ArchiveSessionID: "archive-mem", NativeSessionID: "native-mem", ProjectID: "project-mem",
		Capture:       SourceCapture{Harness: Harness{Name: "codex"}, AdapterName: "codex", AdapterVersion: adapterVersion, SourceFormat: "codex-jsonl", FilterVersion: FilterVersion, CapturedAt: time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)},
		NativeRecords: nativeRecords,
	}
	var plain countingWriter
	if err := EncodeSource(&plain, bundle); err != nil {
		t.Fatal(err)
	}
	compressed, err := BuildCompressedSource(bundle)
	if err != nil {
		t.Fatal(err)
	}
	return compressed, plain
}

type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// The on-disk fixtures directory is where every native shape lives; keep the
// round trip honest if one is added under a new harness prefix.
func TestFixtureBundlesCoverEveryHarnessPrefix(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			// cursor-composer holds synthetic database chats; fixtureBundles
			// round-trips chat.json from it. fuzz holds go test's seed corpora,
			// not native transcripts.
			if entry.Name() != "handoff" && entry.Name() != "cursor-composer" && entry.Name() != "fuzz" {
				t.Errorf("fixture directory %s is not covered by the round-trip test", entry.Name())
			}
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue // e.g. filter-golden.json, which is not a native transcript
		}
		prefix := strings.SplitN(entry.Name(), "-", 2)[0]
		if _, err := NewAdapter(prefix); err != nil {
			t.Errorf("fixture %s has no adapter prefix; the round-trip test would skip it", entry.Name())
		}
	}
}
