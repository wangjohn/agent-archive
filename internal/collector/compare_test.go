package collector

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// jsonEqualByEncoding is what bundleEvidenceEqual used to be for any
// value: encode both, compare the bytes.
func jsonEqualByEncoding(t *testing.T, a, b any) bool {
	t.Helper()
	aBytes, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	bBytes, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(aBytes, bBytes)
}

// jsonValuesEqual answers exactly as comparing encodings does, on the
// values where the two could part: 0 and -0, null and empty containers,
// invalid UTF-8 that encodes alike, and numbers of different Go types.
func TestJSONValuesEqualMatchesEncoding(t *testing.T) {
	t.Parallel()
	values := []any{
		nil, true, false, 0.0, math.Copysign(0, -1), 1.0, 1, int64(1), 1.5, "", "a", "<a>", "\xff", "\xfe", "�",
		map[string]any(nil), map[string]any{}, map[string]any{"a": 1.0}, map[string]any{"a": 1}, map[string]any{"a": 2.0},
		map[string]any{"b": 1.0}, map[string]any{"\xff": 1.0}, map[string]any{"\xfe": 1.0}, map[string]any{"a": 1.0, "b": nil},
		map[string]any{"a": []any{1.0, "x"}}, map[string]any{"a": []any{1.0, "y"}},
		[]any(nil), []any{}, []any{nil}, []any{1.0}, []any{1}, []any{map[string]any{}}, []any{map[string]any(nil)},
		[]string{"a"}, []any{"a"},
	}
	for _, a := range values {
		for _, b := range values {
			got, err := jsonValuesEqual(a, b)
			if err != nil {
				t.Fatalf("%#v, %#v: %v", a, b, err)
			}
			if want := jsonEqualByEncoding(t, a, b); got != want {
				t.Errorf("jsonValuesEqual(%#v, %#v) = %t, encodings equal = %t", a, b, got, want)
			}
		}
	}
}

// Two bundles are the same evidence exactly when their JSON is, but for
// their capture time.
func TestBundleEvidenceEqualMatchesEncoding(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	base := archive.SourceBundle{
		SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: "s", Capture: archive.SourceCapture{CapturedAt: at, FilterVersion: archive.FilterVersion},
		NativeRecords:        []map[string]any{{"type": "message", "n": 1.0, "content": []any{"a", map[string]any{"b": true}}}},
		NativeText:           []archive.TextTranscript{{Format: "cursor-text", Content: "hello"}},
		SupplementalEvidence: []archive.SupplementalEvidence{{Kind: archive.EvidenceKindFinalResponse, ObservedAt: at, Provenance: "hook", Payload: map[string]any{"turn_id": "t"}}},
	}
	variants := map[string]func(b *archive.SourceBundle){
		"same":                    func(*archive.SourceBundle) {},
		"later capture":           func(b *archive.SourceBundle) { b.Capture.CapturedAt = at.Add(time.Hour) },
		"capture in another zone": func(b *archive.SourceBundle) { b.Capture.CapturedAt = at.In(time.FixedZone("x", 3600)) },
		"evidence in another zone": func(b *archive.SourceBundle) {
			b.SupplementalEvidence = []archive.SupplementalEvidence{b.SupplementalEvidence[0]}
			b.SupplementalEvidence[0].ObservedAt = at.In(time.FixedZone("x", 3600))
		},
		"record changed": func(b *archive.SourceBundle) { b.NativeRecords = []map[string]any{{"type": "message", "n": 2.0}} },
		"record added":   func(b *archive.SourceBundle) { b.NativeRecords = append(b.NativeRecords, map[string]any{"type": "x"}) },
		"no records":     func(b *archive.SourceBundle) { b.NativeRecords = []map[string]any{} },
		"nil records":    func(b *archive.SourceBundle) { b.NativeRecords = nil },
		"text changed": func(b *archive.SourceBundle) {
			b.NativeText = []archive.TextTranscript{{Format: "cursor-text", Content: "hello!"}}
		},
		"no text":        func(b *archive.SourceBundle) { b.NativeText = []archive.TextTranscript{} },
		"nil text":       func(b *archive.SourceBundle) { b.NativeText = nil },
		"filter version": func(b *archive.SourceBundle) { b.Capture.FilterVersion += "-next" },
		"evidence added": func(b *archive.SourceBundle) {
			b.SupplementalEvidence = append(b.SupplementalEvidence, b.SupplementalEvidence[0])
		},
		"parent recorded": func(b *archive.SourceBundle) { b.ParentSessionID = "p" },
	}
	encode := func(b archive.SourceBundle) archive.SourceBundle {
		b.Capture.CapturedAt = time.Time{}
		return b
	}
	for name, vary := range variants {
		for _, emptyBase := range []bool{false, true} {
			a := base
			if emptyBase {
				a.NativeRecords, a.NativeText = nil, nil
			}
			b := a
			vary(&b)
			got, err := bundleEvidenceEqual(a, b)
			if err != nil {
				t.Fatal(err)
			}
			if want := jsonEqualByEncoding(t, encode(a), encode(b)); got != want {
				t.Errorf("%s (empty base %t): bundleEvidenceEqual = %t, encodings equal = %t", name, emptyBase, got, want)
			}
		}
	}
}
