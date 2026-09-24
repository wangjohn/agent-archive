package collector

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// bundleEvidenceEqual reports whether two source bundles carry the same
// retained evidence, ignoring their capture timestamp: a changed scan time
// alone must never look like a change in evidence.
func bundleEvidenceEqual(a, b archive.SourceBundle) (bool, error) {
	a.Capture.CapturedAt = time.Time{}
	b.Capture.CapturedAt = time.Time{}
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
// what earlier records look like, and must not read as a rewrite.
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
