package collector

import (
	"errors"
	"fmt"
	"os"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

// skipUnchanged ends a session's turn without reading anything when the
// scan signature shows nothing changed. done reports that it did. A hook
// request always means a read, except on a Cursor database chat whose last
// read failed at the state it is still in: reading it again would copy the
// database only to fail the same way.
func (p *pass) skipUnchanged(reg archive.SessionRegistration, req state.Request) (done bool) {
	id := reg.ArchiveSessionID
	if req.Token != "" && reg.SourceKind != archive.SourceKindCursorSQLite {
		return false
	}
	unchanged, signature, err := p.unchangedSinceLastScan(reg)
	if err != nil {
		p.fail(id, fmt.Errorf("check transcript for changes: %w", err))
		return true
	}
	failed, failure := unchanged && signature.Failed && reg.SourceKind == archive.SourceKindCursorSQLite, ""
	if failed {
		failure = signature.FailedError
	}
	switch {
	case !unchanged, req.Token != "" && !failed:
		return false
	case failure != "":
		// Not read again, but still a failure: reported as one on every
		// pass, as if the read had been repeated. A hook request stays
		// queued, as it does for a transcript file the filter refuses: its
		// evidence is the only copy, and the chat's next change reads the
		// chat again with it.
		p.fail(id, unchangedSinceFailureError{message: failure})
		return true
	}
	// Settled, or a recorded gap at the same state. A request on the gap is
	// completed without a read, as a block completes it: the chat is still
	// over the limit, and the gap stands until the chat changes.
	if req.Token != "" {
		if _, err := p.local.CompleteRequest(id, req.Token); err != nil {
			p.fail(id, fmt.Errorf("complete a request on an unchanged gap: %w", err))
			return true
		}
	}
	p.noteGap(id, signature.Blocked)
	// Nothing to read, nothing to compare, nothing to journal.
	p.result.Skipped = append(p.result.Skipped, id)
	p.opts.progress(id, false)
	return true
}

// unchangedSinceLastScan answers the spec's "transcript changed?" question
// without opening, reading, parsing, or journaling anything: one stat of the
// transcript plus a few tiny local files. It is the difference between a pass
// costing time proportional to every session this machine has ever registered
// and one costing time proportional to the sessions that actually moved.
// signature is the one it compared against.
//
// It says yes only when all of the following hold, because each of them is a
// way an unchanged file can still owe work:
//
//   - The last completed scan left a signature (see ScanSignature): settled,
//     or a recorded gap (Blocked) at the same source state. For a missing
//     transcript that state is its absence, so a transcript that comes back
//     is read at once.
//   - The transcript is a regular file whose size and nanosecond modification
//     time both still match that signature.
//   - The parser, filter, and adapter versions still match, so an upgrade
//     re-derives every session instead of skipping it; and a gap or failure
//     at a size limit stands only while the limits do.
//   - No publication is pending and no interrupted scan is journaled.
//   - A subagent does not owe its parent the notice that it is published
//     (see linkOwed), which is sent from its scan.
//
// The caller has already established that no hook request is pending.
//
// A registration with no transcript path yet has nothing to read, and is
// unchanged whenever it owes no publication either: a hook that reports the
// path changes the registration, which the next pass reads.
//
// The residual risk is a transcript rewritten in place to exactly its previous
// byte length within the same nanosecond. Nanosecond mtimes make that
// essentially unreachable for a real application, but it is not a proof, so a
// Cursor text transcript — which has no per-record timestamps and is compared
// by prefix, making a silent in-place edit hardest to detect downstream — is
// never skipped on a stat alone while it exists.
//
// A Cursor database chat is identified by its cursorstore.Signature instead
// of a stat (see sourceReader).
//
// A Cursor chat whose last read failed in a way reading it again can't fix
// (see rememberFailedRead) is also "unchanged" while its signature and the
// versions match, although that failed scan is still journaled; Run reports
// its failure again without reading it.
func (p *pass) unchangedSinceLastScan(reg archive.SessionRegistration) (unchanged bool, signature state.ScanSignature, err error) {
	id := reg.ArchiveSessionID
	if owed, err := p.linkOwed(reg); err != nil || owed {
		return false, signature, err
	}
	reader, ok := newSourceReader(reg, p.opts)
	if !ok {
		return p.owesNothing(id, false)
	}
	signature, found, err := p.local.LoadScanSignature(id)
	if err != nil || !found {
		return false, signature, err
	}
	if signature.SourceFormat == cursorTextSourceFormat && signature.Blocked != state.BlockedReasonTranscriptMissing {
		return false, signature, nil
	}
	adapterVersion, known := harnessAdapterVersion(reg.Harness.Name)
	if !known {
		return false, signature, nil
	}
	if signature.ParserVersion != p.opts.parserVersion() || signature.FilterVersion != archive.FilterVersion || signature.AdapterVersion != adapterVersion {
		return false, signature, nil
	}
	if (signature.Failed || sizeLimitGap(signature.Blocked)) && (signature.FailedMaxBytes != p.opts.maxTranscriptBytes() || signature.FailedRecordLimit != recordLimit) {
		return false, signature, nil
	}
	observed, observeErr := reader.Signature(p.ctx)
	switch {
	case signature.Blocked == state.BlockedReasonTranscriptMissing:
		if !errors.Is(observeErr, os.ErrNotExist) {
			return false, signature, nil
		}
	case observeErr != nil || !observed.matches(signature):
		return false, signature, nil
	}
	unchanged, _, err = p.owesNothing(id, signature.Failed)
	return unchanged, signature, err
}

// owesNothing reports that a session has no publication pending and, unless
// failed (a remembered Cursor failure, whose scan stays journaled), no
// interrupted scan journaled.
func (p *pass) owesNothing(id string, failed bool) (bool, state.ScanSignature, error) {
	if !failed {
		if scanPending, err := p.local.ScanPending(id); err != nil || scanPending {
			return false, state.ScanSignature{}, err
		}
	}
	pending, err := p.local.HasPending(id)
	if err != nil || pending {
		return false, state.ScanSignature{}, err
	}
	return true, state.ScanSignature{}, nil
}

// sizeLimitGap reports a gap recorded at a size limit, which stands only
// while the limits do.
func sizeLimitGap(reason state.BlockedReason) bool {
	return reason == state.BlockedReasonTranscriptTooLarge || reason == state.BlockedReasonRecordTooLarge
}

// noteGap counts a session sitting in a size-limit gap, for status.
func (p *pass) noteGap(id string, reason state.BlockedReason) {
	if sizeLimitGap(reason) {
		p.sizeLimited = append(p.sizeLimited, id)
	}
}

// linkOwed reports a subagent that is published while its parent, still
// registered and not blocked, does not link it as published yet: its scan
// sends the parent that notice (announceSubagent), so it is not skipped. A
// blocked parent drops the notice it acknowledges, and is announced to once
// it is unblocked. Only the two sessions' summaries are read, the parent's
// once a pass however many subagents it has.
func (p *pass) linkOwed(reg archive.SessionRegistration) (bool, error) {
	parent := reg.ParentSessionID
	if parent == "" || !p.registered[parent] {
		return false, nil
	}
	child, found, err := p.local.LoadPublishedSummary(reg.ArchiveSessionID)
	if err != nil || !found || !child.Published {
		return false, err
	}
	summary, seen := p.parents[parent]
	if !seen {
		if summary, _, err = p.local.LoadPublishedSummary(parent); err != nil {
			return false, err
		}
		p.parents[parent] = summary
	}
	return summary.Status != state.CacheStatusBlocked && !summary.LinksPublished(reg.ArchiveSessionID), nil
}

// recordScanSignature marks a session settled at the transcript bytes this
// scan consumed, so the next pass can skip it. It is written only at an exit
// that owes no further work.
func (s *sessionScan) recordScanSignature(observed sourceState, bundle archive.SourceBundle) error {
	return s.local.SaveScanSignature(s.id(), state.ScanSignature{
		TranscriptSize: observed.file.Size, TranscriptMtime: observed.file.Mtime,
		ParserVersion: s.opts.parserVersion(), FilterVersion: bundle.Capture.FilterVersion,
		AdapterVersion: bundle.Capture.AdapterVersion, SourceFormat: bundle.Capture.SourceFormat,
		SourceKind: observed.kind, CursorLastUpdatedAt: observed.cursor.LastUpdatedAt,
		CursorHeaderCount: observed.cursor.HeaderCount, CursorLastBubbleID: observed.cursor.LastBubbleID,
		CursorMessageRows: observed.cursor.MessageRows, CursorLastMessageHash: observed.cursor.LastMessageHash,
	})
}

// recordBlockedSignature marks a session sitting in a recorded gap at the
// source state observed, so the next pass skips it until that state changes
// instead of reading it again to reach the same gap. Without an observed
// state (nil) nothing can vouch for the gap, and the signature is removed:
// the session is scanned again.
func (s *sessionScan) recordBlockedSignature(reason state.BlockedReason, observed *sourceState) error {
	adapterVersion, known := harnessAdapterVersion(s.reg.Harness.Name)
	if observed == nil || !known {
		return s.local.RemoveScanSignature(s.id())
	}
	return s.local.SaveScanSignature(s.id(), state.ScanSignature{
		TranscriptSize: observed.file.Size, TranscriptMtime: observed.file.Mtime,
		ParserVersion: s.opts.parserVersion(), FilterVersion: archive.FilterVersion, AdapterVersion: adapterVersion,
		SourceKind: observed.kind, CursorLastUpdatedAt: observed.cursor.LastUpdatedAt,
		CursorHeaderCount: observed.cursor.HeaderCount, CursorLastBubbleID: observed.cursor.LastBubbleID,
		CursorMessageRows: observed.cursor.MessageRows, CursorLastMessageHash: observed.cursor.LastMessageHash,
		FailedMaxBytes: s.opts.maxTranscriptBytes(), FailedRecordLimit: recordLimit,
		Blocked: reason,
	})
}

// missingSource is the observed state of a source that is gone: nothing but
// its kind, recorded as recordScanSignature records it (empty for a file).
func missingSource(reg archive.SessionRegistration) *sourceState {
	if reg.SourceKind == archive.SourceKindCursorSQLite {
		return &sourceState{kind: archive.SourceKindCursorSQLite}
	}
	return &sourceState{}
}
