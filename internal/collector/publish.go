package collector

import (
	"errors"
	"fmt"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// blockThenRemember is block for a size limit, which for a Cursor chat also
// remembers the state it was reached at (see rememberFailedRead).
func (s *sessionScan) blockThenRemember(reason state.BlockedReason, read sourceRead) (sessionOutcome, error) {
	outcome, err := s.block(reason, nil)
	if err != nil {
		return outcome, err
	}
	return outcome, rememberFailedRead(s.local, s.reg, read.adapter, read.observed, s.opts, nil)
}

// blockSession is sessionScan.block for one session outside a scan.
func blockSession(local *state.Store, id string, req state.Request, reason state.BlockedReason, candidate *archive.SourceBundle) (sessionOutcome, error) {
	published, err := local.LoadPublishedState(id)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("load published cache: %w", err)
	}
	scan := &sessionScan{local: local, reg: archive.SessionRegistration{ArchiveSessionID: id}, req: req, published: published}
	return scan.block(reason, candidate)
}

// block records a terminal capture gap for the session and completes its
// request, so the pass ends cleanly and the session is not counted as
// pending. candidate, when known, becomes the cached comparison bundle so an
// unchanged transcript is skipped on the next pass; otherwise the previously
// cached bundle (or the last published one) is kept. The last published
// snapshot is retained untouched either way.
func (s *sessionScan) block(reason state.BlockedReason, candidate *archive.SourceBundle) (sessionOutcome, error) {
	cached, _, _, haveCached := s.published.Cached()
	lastPublished, lastPublishedAt, _ := s.published.LastPublished()
	bundle := lastPublished
	if haveCached {
		bundle = cached
	}
	if candidate != nil {
		bundle = *candidate
	}
	previousReason, blocked := s.published.Blocked()
	alreadyBlocked := blocked && previousReason == reason && candidate == nil
	// A block that can end holds on to the hook evidence its acknowledgement
	// would otherwise discard, so a final response that arrived just before
	// the application deleted its transcript still publishes if the file
	// comes back. A permanent block has nothing to hand it back to.
	var held []archive.SupplementalEvidence
	if reason.Recoverable() {
		held = s.req.HookEvidence
	}
	if !alreadyBlocked || len(held) > 0 {
		if err := s.published.SaveBlocked(bundle, lastPublishedAt, reason, held...); err != nil {
			return outcomeSkipped, fmt.Errorf("cache blocked session: %w", err)
		}
	}
	// A recorded gap is re-evaluated on every pass, never skipped on a stat:
	// the condition that caused it (a deleted file above all) can end without
	// the transcript itself changing.
	if err := s.local.RemoveScanSignature(s.id()); err != nil {
		return outcomeSkipped, err
	}
	return outcomeSkipped, s.completeRequest("complete blocked request")
}

// publishPending uploads a pending publication and records it: source then
// metadata (or, for a metadata-only publication that carries no source
// bytes, a check of the recorded source then metadata), the superseded
// source in the ledger, the new published state, the covered request, and
// finally the removal of the pending file.
func (s *sessionScan) publishPending(pending state.PendingPublication) (sessionOutcome, error) {
	if !pending.CarriesNoSource() && !storage.VerifySHA256(pending.SourceBytes, pending.SourceSHA256) {
		return outcomeSkipped, errors.New("pending source checksum does not match its persisted bytes")
	}
	pending.Attempted = true
	if err := s.local.SavePending(s.id(), pending); err != nil {
		return outcomeSkipped, fmt.Errorf("mark pending publication attempted: %w", err)
	}
	if err := s.upload(pending); err != nil {
		return outcomeSkipped, err
	}
	// The object this publication replaced is the one recorded when it was
	// uploaded, never one rebuilt from its bundle now (see
	// state.Published.LastPublishedSource). If it is unknown, only state from
	// an old version without cached metadata, nothing is recorded: the old
	// object then stays until the whole session expires, which is safe.
	if previous, hadPrevious := s.published.LastPublishedSource(); hadPrevious && previous.Key != pending.SourceKey {
		if err := s.local.RecordSuperseded(s.id(), previous.Key, s.now); err != nil {
			return outcomeSkipped, fmt.Errorf("record superseded source: %w", err)
		}
	}
	var saveErr error
	if pending.MetadataOnly {
		saveErr = s.published.SaveRepublishedMetadata(pending, s.now)
	} else {
		saveErr = s.published.SavePublication(pending.Bundle, s.now, pending.SourceReference(), pending.MetadataBytes)
	}
	if err := saveErr; err != nil {
		return outcomeSkipped, fmt.Errorf("update published cache: %w", err)
	}
	// Whatever kept this session's metadata from being refreshed described
	// the publication just replaced.
	if err := s.local.RemoveRefreshSkip(s.id()); err != nil {
		return outcomeSkipped, err
	}
	if pending.RequestToken != "" {
		if _, err := s.local.CompleteRequest(s.id(), pending.RequestToken); err != nil {
			return outcomeSkipped, fmt.Errorf("complete published request: %w", err)
		}
	}
	if err := s.local.RemovePending(s.id()); err != nil {
		return outcomeSkipped, err
	}
	return outcomePublished, nil
}

// upload writes a pending publication to storage.
func (s *sessionScan) upload(pending state.PendingPublication) error {
	if !pending.CarriesNoSource() {
		if err := storage.PutSourceThenMetadata(s.ctx, s.remote, pending.SourceKey, pending.MetadataKey, pending.SourceBytes, pending.MetadataBytes, s.opts.Retry); err != nil {
			return fmt.Errorf("publish: %w", err)
		}
		return nil
	}
	err := storage.PutMetadataForSource(s.ctx, s.remote, pending.SourceKey, pending.SourceSHA256, pending.SourceSize, pending.MetadataKey, pending.MetadataBytes, s.opts.Retry)
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrChecksumMismatch) {
		// The recorded source is not in storage as recorded, and without its
		// bytes this publication can never succeed. Dropping it keeps it from
		// holding back normal capture; the metadata still points at whatever
		// it pointed at before. If this build can build the bundle after all,
		// the next refresh publishes it as a new source; if not, the refresh
		// is recorded as impossible so it is not retried (with a download)
		// on every pass.
		if removeErr := s.local.RemovePending(s.id()); removeErr != nil {
			err = errors.Join(err, removeErr)
		}
		if _, buildErr := archive.BuildCompressedSource(pending.Bundle); buildErr != nil {
			skip := state.RefreshSkip{ParserVersion: s.opts.parserVersion(), SourceKey: pending.SourceKey, Reason: state.RefreshSkipSourceUnavailable}
			if skipErr := s.local.SaveRefreshSkip(s.id(), skip); skipErr != nil {
				err = errors.Join(err, skipErr)
			}
		}
	}
	if err != nil {
		return fmt.Errorf("publish metadata: %w", err)
	}
	return nil
}
