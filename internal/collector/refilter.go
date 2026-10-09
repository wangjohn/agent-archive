package collector

import (
	"context"
	"errors"
	"fmt"

	"github.com/wangjohn/agent-archive/internal/agentapi"
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
	semantics, err := sourceSemantics(s.opts.Sources, s.reg)
	if err != nil {
		return false, err
	}
	if semantics.Mutation != agentapi.AppendOnly {
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
	return read.observed.size() < signature.TranscriptSize, nil
}

// refilterRewritten replaces candidate, a rewritten transcript's evidence
// filtered by a new filter or adapter version, with the retained snapshot
// filtered again by the current rules. replaced reports that it did. A
// snapshot that cannot be filtered again leaves candidate to replace it, as
// before this existed, with a warning: the new filter's output, though
// poorer, is still safer to publish than the old filter's.
func (s *sessionScan) refilterRewritten(_ context.Context, read sourceRead, snapshot, candidate archive.SourceBundle) (_ archive.SourceBundle, replaced bool, err error) {
	rewritten, err := s.rewrittenSinceCapture(read)
	if err != nil || !rewritten {
		return candidate, false, err
	}
	refiltered, err := s.refilterRetained(read.adapter, snapshot)
	if err != nil {
		if errors.Is(err, agentapi.ErrReadBudget) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return candidate, false, err
		}
		s.warn(fmt.Errorf("filter the retained snapshot of a rewritten transcript again (the rewritten transcript replaces it): %w", err))
		return candidate, false, nil
	}
	if nativeEvidenceExtends(read.adapter, refiltered, candidate) {
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
		// records are not captured until the user explicitly starts a linked
		// generation with agent-archive recover. No retained evidence is lost. A looser
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

// Native parent links may resolve after a retained revision was captured. The
// stable child owner still identifies that earlier evidence; a different known
// parent remains a conflict.
func retainedParentMatches(reg archive.SessionRegistration, bundle archive.SourceBundle) bool {
	return bundle.ParentSessionID == reg.ParentSessionID || nativeChildOwned(reg, bundle) && bundle.ParentSessionID == ""
}

// Only positively identified native child evidence can acquire its first archive parent.
func nativeParentResolved(reg archive.SessionRegistration, bundle archive.SourceBundle) bool {
	return nativeChildOwned(reg, bundle) && bundle.ParentSessionID == "" && reg.ParentSessionID != ""
}

// A missing optional marker is unknown on older sources. Only the same already
// admitted owner and a supported persisted child binding may upgrade it; parent
// links and current native observations cannot supply that proof.
func nativeChildOwned(reg archive.SessionRegistration, bundle archive.SourceBundle) bool {
	return agentapi.RetainedNativeChildOwned(reg, bundle)
}

func nativeChildMarkerPending(reg archive.SessionRegistration, bundle archive.SourceBundle) bool {
	return !bundle.NativeChild && nativeChildOwned(reg, bundle)
}
