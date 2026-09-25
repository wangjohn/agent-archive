package collector

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

// A new filter or adapter version changes what earlier records look like, so
// the rewrite guard cannot compare a snapshot filtered by an earlier release
// with one this release filters (see nativeEvidenceExtends). Most of the
// time that is harmless: the transcript still holds everything, and the
// new-filter candidate replaces the old snapshot with the same evidence
// filtered by the current rules. A transcript that was truncated or
// rewritten since it was captured does not: its candidate is poorer than the
// snapshot. Publishing it would lose the rest for good, and keeping the old
// snapshot would leave content an earlier, weaker filter kept as the
// session's current copy. Instead the snapshot itself is filtered again by
// the current rules and republished, and the rewrite is recorded as the gap
// it is.

// versionChanged reports whether a and b were filtered by different filter
// or adapter versions.
func versionChanged(a, b archive.SourceBundle) bool {
	return a.Capture.FilterVersion != b.Capture.FilterVersion || a.Capture.AdapterVersion != b.Capture.AdapterVersion
}

// rewrittenSinceCapture reports whether a transcript file no longer holds
// everything the retained snapshot was captured from: the session already
// stands in a transcript_rewritten gap, or the file is smaller than at the
// scan that last settled it. A JSONL transcript only grows, so a smaller
// file has lost records; a rewrite that does not shrink the file is not
// detected here. Cursor database chats are never reported: a rewritten chat
// is published as the chat now is (see guard).
func (s *sessionScan) rewrittenSinceCapture(read sourceRead) (bool, error) {
	if s.reg.SourceKind != archive.SourceKindFile {
		return false, nil
	}
	if reason, blocked := s.published.Blocked(); blocked && reason == state.BlockedReasonTranscriptRewritten {
		return true, nil
	}
	signature, found, err := s.local.LoadScanSignature(s.id())
	if err != nil {
		return false, fmt.Errorf("load scan signature: %w", err)
	}
	if !found || signature.Blocked != "" || signature.Failed || signature.SourceKind != read.observed.kind {
		return false, nil
	}
	return read.observed.file.Size < signature.TranscriptSize, nil
}

// refilterBundle filters a retained snapshot's records again with adapter
// (the current filter), keeping its capture time, its supplemental evidence
// (filtered again by NewSourceBundle), and its capture gaps. Filtered output
// is valid filter input and refilters unchanged under the same rules
// (FuzzFilterJSONL, FuzzCursorText), so under newer rules only what they
// now drop or redact changes. What the earlier filter already dropped stays
// dropped.
func refilterBundle(reg archive.SessionRegistration, adapter archive.Adapter, bundle archive.SourceBundle) (archive.SourceBundle, error) {
	filtered, err := refilterNative(reg, adapter, bundle)
	if err != nil {
		return archive.SourceBundle{}, err
	}
	refiltered, err := archive.NewSourceBundle(reg, adapter, filtered, bundle.Capture.CapturedAt, bundle.SupplementalEvidence)
	if err != nil {
		return archive.SourceBundle{}, err
	}
	refiltered.Capture.Gaps = mergeCaptureGaps(bundle.Capture.Gaps, refiltered.Capture.Gaps)
	return refiltered, nil
}

// refilterNative runs a snapshot's native records, or its native text, back
// through the filter.
func refilterNative(reg archive.SessionRegistration, adapter archive.Adapter, bundle archive.SourceBundle) (archive.FilteredTranscript, error) {
	if len(bundle.NativeText) > 0 {
		cursor, ok := adapter.(archive.CursorAdapter)
		if !ok || len(bundle.NativeText) != 1 || len(bundle.NativeRecords) > 0 {
			return archive.FilteredTranscript{}, errors.New("snapshot text cannot be filtered again by this adapter")
		}
		return cursor.FilterText(strings.NewReader(bundle.NativeText[0].Content), reg.SessionStartedAt)
	}
	var jsonl bytes.Buffer
	for i, record := range bundle.NativeRecords {
		encoded, err := json.Marshal(record)
		if err != nil {
			return archive.FilteredTranscript{}, fmt.Errorf("encode retained record %d: %w", i, err)
		}
		if i > 0 {
			jsonl.WriteByte('\n')
		}
		jsonl.Write(encoded)
	}
	return adapter.FilterJSONL(&jsonl)
}

// mergeCaptureGaps is first followed by each gap of second not already in
// it: the snapshot's own gaps, which filtering it again cannot rediscover
// (they describe the raw transcript), plus any the new filter reports.
func mergeCaptureGaps(first, second []archive.CaptureGap) []archive.CaptureGap {
	out := append([]archive.CaptureGap(nil), first...)
	for _, gap := range second {
		if !slices.Contains(out, gap) {
			out = append(out, gap)
		}
	}
	return out
}

// refilterRewritten replaces candidate, a rewritten transcript's evidence
// filtered by a new filter or adapter version, with the retained snapshot
// filtered again by the current rules. replaced reports that it did. A
// snapshot that cannot be filtered again leaves candidate to replace it, as
// before this existed, with a warning: the new filter's output, though
// poorer, is still safer to publish than the old filter's.
func (s *sessionScan) refilterRewritten(read sourceRead, snapshot, candidate archive.SourceBundle) (_ archive.SourceBundle, replaced bool, err error) {
	rewritten, err := s.rewrittenSinceCapture(read)
	if err != nil || !rewritten {
		return candidate, false, err
	}
	refiltered, err := refilterBundle(s.reg, read.adapter, snapshot)
	if err != nil {
		s.warn(fmt.Errorf("filter the retained snapshot of a rewritten transcript again (the rewritten transcript replaces it): %w", err))
		return candidate, false, nil
	}
	if nativeEvidenceExtends(refiltered, candidate) {
		// Filtered the same way now, the two compare record for record, and
		// the transcript holds everything the snapshot did after all (a
		// file restored since its rewrite gap was recorded).
		//
		// Known limitation: refiltering the snapshot starts from what the
		// earlier filter wrote, and the candidate from the raw transcript.
		// Where the new rules change a record's output (a new redaction
		// label, a field the new adapter keeps), a restored transcript does
		// not compare equal: the snapshot is republished, the gap stands,
		// and later passes compare the same way, so the transcript's new
		// records are not captured. No retained evidence is lost. A looser
		// check (record counts, file size) cannot tell a restored file from
		// one compacted and then grown, and would publish the latter over
		// the richer snapshot, the loss this exists to prevent.
		// TestFilterUpgradeKeepsTheSnapshotOfARestoredTranscriptItCannotMatch
		// pins it.
		return candidate, false, nil
	}
	s.rewritten = &candidate
	return refiltered, true, nil
}
