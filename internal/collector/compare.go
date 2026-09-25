package collector

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// bundleEvidenceEqual reports whether two source bundles carry the same
// retained evidence, ignoring their capture timestamp: a changed scan time
// alone must never look like a change in evidence. Evidence is equal when
// its JSON is. The native records and text, nearly all of a large bundle,
// are compared in place (see jsonValuesEqual) rather than by encoding both
// bundles, which cost two copies of a bundle tens of megabytes long every
// pass; the rest is small and is compared as JSON.
func bundleEvidenceEqual(a, b archive.SourceBundle) (bool, error) {
	if len(a.NativeRecords) != len(b.NativeRecords) || (a.NativeRecords == nil) != (b.NativeRecords == nil) || len(a.NativeText) != len(b.NativeText) {
		return false, nil
	}
	for i := range a.NativeText {
		if !jsonStringsEqual(a.NativeText[i].Format, b.NativeText[i].Format) || !jsonStringsEqual(a.NativeText[i].Content, b.NativeText[i].Content) {
			return false, nil
		}
	}
	for i := range a.NativeRecords {
		if same, err := jsonValuesEqual(a.NativeRecords[i], b.NativeRecords[i]); err != nil || !same {
			return false, err
		}
	}
	a.Capture.CapturedAt, b.Capture.CapturedAt = time.Time{}, time.Time{}
	a.NativeRecords, b.NativeRecords = nil, nil
	a.NativeText, b.NativeText = nil, nil
	return jsonEncodingsEqual(a, b)
}

// jsonValuesEqual reports whether a and b encode to the same JSON, without
// encoding them when they hold what decoded JSON holds (objects, arrays,
// strings, numbers, booleans, null) of the same kinds. Anything else is
// encoded and compared.
func jsonValuesEqual(a, b any) (bool, error) {
	switch x := a.(type) {
	case map[string]any:
		if y, ok := b.(map[string]any); ok {
			if (x == nil) != (y == nil) || len(x) != len(y) {
				return false, nil
			}
			for key, xv := range x {
				yv, found := y[key]
				if !found {
					// Keys that differ as strings can still encode alike
					// (invalid UTF-8): only an encoding can tell.
					return jsonEncodingsEqual(a, b)
				}
				if same, err := jsonValuesEqual(xv, yv); err != nil || !same {
					return false, err
				}
			}
			return true, nil
		}
	case []any:
		if y, ok := b.([]any); ok {
			if (x == nil) != (y == nil) || len(x) != len(y) {
				return false, nil
			}
			for i := range x {
				if same, err := jsonValuesEqual(x[i], y[i]); err != nil || !same {
					return false, err
				}
			}
			return true, nil
		}
	case string:
		if y, ok := b.(string); ok {
			return jsonStringsEqual(x, y), nil
		}
	case float64:
		// Every float64 JSON can hold has one encoding, and 0 and -0 have
		// different ones ("0", "-0"), as they have different bits.
		if y, ok := b.(float64); ok {
			return math.Float64bits(x) == math.Float64bits(y), nil
		}
	case bool:
		if y, ok := b.(bool); ok {
			return x == y, nil
		}
	case nil:
		if b == nil {
			return true, nil
		}
	}
	return jsonEncodingsEqual(a, b)
}

// jsonStringsEqual reports whether two strings encode to the same JSON.
// Distinct valid UTF-8 strings never do; invalid bytes encode as U+FFFD, so
// strings holding them are compared by encoding.
func jsonStringsEqual(a, b string) bool {
	if a == b {
		return true
	}
	if utf8.ValidString(a) && utf8.ValidString(b) {
		return false
	}
	same, err := jsonEncodingsEqual(a, b)
	return err == nil && same
}

func jsonEncodingsEqual(a, b any) (bool, error) {
	aBytes, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	bBytes, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(aBytes, bBytes), nil
}

// bundleChangeIsLinkOnly reports whether the only difference between the last
// snapshot and the candidate is which child sessions the parent links to. A
// link is a note about another session, not new activity in this one, so it
// must not restart the parent's retention clock (retention.go measures from
// Capture.CapturedAt).
func bundleChangeIsLinkOnly(a, b archive.SourceBundle) (bool, error) {
	a.LinkedSessions, b.LinkedSessions = nil, nil
	a.SupplementalEvidence = withoutLinkedSessionEvidence(a.SupplementalEvidence)
	b.SupplementalEvidence = withoutLinkedSessionEvidence(b.SupplementalEvidence)
	return bundleEvidenceEqual(a, b)
}

func withoutLinkedSessionEvidence(in []archive.SupplementalEvidence) []archive.SupplementalEvidence {
	out := make([]archive.SupplementalEvidence, 0, len(in))
	for _, item := range in {
		if item.Kind == archive.EvidenceKindLinkedSession {
			continue
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// nativeEvidenceExtends reports whether candidate carries everything previous
// did, record for record, so replacing previous loses no retained evidence.
// It compares filtered output, so it is only meaningful when both were
// filtered the same way: a new filter or adapter version legitimately changes
// what earlier records look like, and must not read as a rewrite. A rewrite
// across an upgrade is found from the transcript instead (see refilter.go).
func nativeEvidenceExtends(previous, candidate archive.SourceBundle) bool {
	if previous.Capture.FilterVersion != candidate.Capture.FilterVersion || previous.Capture.AdapterVersion != candidate.Capture.AdapterVersion {
		return true
	}
	if previous.Capture.SourceFormat != candidate.Capture.SourceFormat || len(candidate.NativeRecords) < len(previous.NativeRecords) || len(candidate.NativeText) < len(previous.NativeText) {
		return false
	}
	for i := range previous.NativeRecords {
		if !reflect.DeepEqual(previous.NativeRecords[i], candidate.NativeRecords[i]) {
			return false
		}
	}
	for i := range previous.NativeText {
		if previous.NativeText[i].Format != candidate.NativeText[i].Format || !strings.HasPrefix(candidate.NativeText[i].Content, previous.NativeText[i].Content) {
			return false
		}
	}
	return true
}

func mergeSupplementalEvidence(existing []archive.SupplementalEvidence, groups ...[]archive.SupplementalEvidence) []archive.SupplementalEvidence {
	out := existing
	for _, additions := range groups {
		out = archive.MergeSupplementalEvidence(out, additions)
	}
	return out
}
