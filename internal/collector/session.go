package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// One scan of one session (sessionScan.run) goes through these steps, each
// a method of sessionScan below:
//
//	resume   A pending publication (bytes frozen before its first upload) is
//	         retried as it is, unless it was never attempted and a new hook
//	         request may replace it with a richer one.
//	refresh  With nothing pending, a parser upgrade re-derives the last
//	         publication's metadata from the retained bundle
//	         (regenerateMetadata), without reading the transcript.
//	read     The source (a transcript file, or a Cursor database chat) is
//	         read and filtered. A size limit or a deleted transcript ends the
//	         scan as a recorded gap (blocked).
//	build    The candidate bundle: the filtered records plus supplemental
//	         evidence (hook evidence, and skill observations when the
//	         session was active).
//	compare  Against the cached bundle. Unchanged settles the scan: the
//	         request is completed and the scan signature recorded. Changed
//	         is a new snapshot, captured now.
//	guard    A candidate that no longer extends what was published is a
//	         rewrite: a transcript is blocked; a Cursor chat is accepted with
//	         a rewrite gap.
//	decide   Decline it (Options.RequireSkillUse), hold it back
//	         (Options.MinUploadInterval), or publish it now.
//	persist  The publication is saved as pending before any upload, then
//	         published (publishPending) or cached as rate limited.
//
// The cached bundle's status (state.CacheStatus) moves between:
//
//	none, declined, rate_limited ── new evidence, interval passed ──▶ published
//	published ── new evidence within the interval ──▶ rate_limited
//	any ── new evidence, policy says no ──▶ declined
//	any ── size limit, missing transcript, rewrite ──▶ blocked
//	blocked(transcript_missing) ── transcript back ──▶ the status before it
//
// A publication itself is never undone locally: the last published snapshot
// and its source reference survive every other status.

// sessionScan is one scan of one session: what it works from, each read
// once. published is the session's published state; every step reads and
// saves it through this one copy rather than decoding published/<id>.json
// again (it holds whole source bundles).
type sessionScan struct {
	ctx       context.Context
	local     *state.Store
	remote    storage.ObjectStore
	opts      Options
	now       time.Time
	reg       archive.SessionRegistration
	req       state.Request
	published *state.Published
	// readyAt is when a publication the scan left waiting for the upload
	// interval (outcomeRateLimited) becomes due.
	readyAt time.Time
	// warnings are failures that did not stop the scan or change its
	// outcome, reported with the session once it ends.
	warnings []error
	// gap is the capture gap the scan ended in, if it did (see block).
	gap state.BlockedReason
}

// warn records a failure that does not end the scan.
func (s *sessionScan) warn(err error) { s.warnings = append(s.warnings, err) }

func newSessionScan(ctx context.Context, local *state.Store, remote storage.ObjectStore, reg archive.SessionRegistration, req state.Request, published *state.Published, now time.Time, opts Options) *sessionScan {
	return &sessionScan{ctx: ctx, local: local, remote: remote, opts: opts, now: now, reg: reg, req: req, published: published}
}

type sessionOutcome int

const (
	outcomeSkipped sessionOutcome = iota
	outcomeRateLimited
	outcomePublished
)

func (s *sessionScan) id() string { return s.reg.ArchiveSessionID }

func (s *sessionScan) run() (sessionOutcome, error) {
	if outcome, handled, err := s.resume(); handled || err != nil {
		return outcome, err
	}
	read, ok, err := s.read()
	if !ok || err != nil {
		return read.outcome, err
	}
	candidate, supplemental, err := s.build(read)
	if err != nil {
		return outcomeSkipped, err
	}
	if settled, err := s.compare(read, &candidate); settled || err != nil {
		return outcomeSkipped, err
	}
	candidate, blocked, err := s.guard(read, candidate, supplemental)
	if blocked || err != nil {
		return outcomeSkipped, err
	}
	return s.publish(read, candidate)
}

// resume retries a pending publication, or runs the metadata refresh when
// there is none. handled reports that the scan ends here.
func (s *sessionScan) resume() (outcome sessionOutcome, handled bool, err error) {
	// A publication that may already have reached storage is immutable local
	// work. Retry its exact bytes before considering later transcript changes.
	pending, havePending, err := s.local.LoadPending(s.id())
	if err != nil {
		return outcomeSkipped, true, err
	}
	if !havePending {
		return regenerateMetadata(s)
	}
	if !pending.Attempted && s.req.Token != "" && s.req.Token != pending.RequestToken {
		// A stop/end request is a natural debounce flush. A merely rate-limited,
		// never-attempted candidate can be safely replaced by a richer one;
		// deferred lifecycle evidence enriches it without changing when it
		// becomes ready.
		return outcomeSkipped, false, nil
	}
	// No publication ever waits longer than one interval from now. A later
	// ReadyAt came from a clock that was wrong when it was set (see
	// publicationReadyAt); it is capped durably, since a cap recomputed from
	// each pass's now would keep moving away.
	if latest := s.now.Add(s.opts.minUploadInterval()); pending.ReadyAt.After(latest) {
		pending.ReadyAt = latest
		if err := s.local.SavePending(s.id(), pending); err != nil {
			return outcomeSkipped, true, fmt.Errorf("cap pending publication time: %w", err)
		}
	}
	if !pending.ReadyAt.IsZero() && s.now.Before(pending.ReadyAt) {
		s.readyAt = pending.ReadyAt
		return outcomeRateLimited, true, nil
	}
	outcome, err = s.publishPending(pending)
	return outcome, true, err
}

// sourceRead is what reading the session's source produced.
type sourceRead struct {
	adapter  archive.Adapter
	filtered archive.FilteredTranscript
	observed sourceState
	// outcome is the scan's outcome when the read ended it.
	outcome sessionOutcome
}

// read reads and filters the session's source. ok is false when the scan
// ends here: nothing to read yet, or a recorded gap, or an error.
func (s *sessionScan) read() (read sourceRead, ok bool, err error) {
	reader, available := newSourceReader(s.reg, s.opts)
	if !available {
		// Not an error: a Cursor desktop chat is registered at its first
		// prompt, before Cursor names its transcript, and a later hook fills
		// the path in. Until then there is nothing to read. Any hook request
		// stays queued, so the session is scanned again each pass while one
		// is (otherwise unchangedSinceLastScan skips it until the path
		// arrives), and nothing is recorded as a failure.
		return read, false, nil
	}
	if read.adapter, err = archive.NewAdapter(s.reg.Harness.Name); err != nil {
		return read, false, err
	}
	read.filtered, read.observed, err = reader.Filter(s.ctx, read.adapter, s.opts.maxTranscriptBytes())
	if err != nil {
		read.outcome, err = s.readFailed(read, err)
		return read, false, err
	}
	if read.observed.empty() {
		// The application has created the file but written no record yet:
		// the fresh-start proof accepts exactly this state, so it is the
		// same waiting as having no path at all, one step later. Nothing has
		// been captured, so there is nothing to block or fail; the request
		// stays queued and the next pass reads whatever has arrived. A file
		// emptied after a publication is a rewrite, which the comparison
		// still reports rather than waits on.
		if _, _, published := s.published.LastPublished(); !published {
			return read, false, nil
		}
	}
	if err := validateSubagentTranscript(s.reg, read.filtered); err != nil {
		return read, false, err
	}
	return read, true, nil
}

// readFailed turns a failed read into the scan's end: a recorded gap for a
// condition retrying cannot fix, otherwise the error.
func (s *sessionScan) readFailed(read sourceRead, err error) (sessionOutcome, error) {
	switch {
	case errors.Is(err, errTranscriptTooLarge):
		// The file will not shrink by retrying: record the gap once and
		// keep the last published snapshot instead of failing every pass.
		return s.blockThenRemember(state.BlockedReasonTranscriptTooLarge, read)
	case errors.Is(err, errRecordTooLarge):
		// Likewise one record too long to read: a gap recorded once, the
		// last published snapshot kept, cleared when the file changes.
		return s.blockThenRemember(state.BlockedReasonRecordTooLarge, read)
	case errors.Is(err, os.ErrNotExist):
		// The application deleted its own transcript. Retention keeps
		// sessions for far longer than any of them keep their logs, so
		// this is the ordinary end state of every archived session, not a
		// failure: record it as a capture gap once, keep the last
		// published snapshot, and let a returning file clear it. Until it
		// returns, each pass skips the session on a stat that finds it
		// still missing.
		return s.block(state.BlockedReasonTranscriptMissing, nil, missingSource(s.reg))
	}
	// Unsafe format: never upload; the last published snapshot, if any,
	// remains untouched and readable.
	err = fmt.Errorf("filter transcript: %w", err)
	if rememberErr := rememberFailedRead(s.local, s.reg, read.adapter, read.observed, s.opts, err, ""); rememberErr != nil {
		return outcomeSkipped, errors.Join(err, rememberErr)
	}
	return outcomeSkipped, err
}

// build assembles the candidate bundle, and returns the supplemental
// evidence it was built with. A recoverable block the readable
// source has ended is cleared first, since a source that comes back carrying
// exactly what was already captured produces no change to act on.
func (s *sessionScan) build(read sourceRead) (archive.SourceBundle, []archive.SupplementalEvidence, error) {
	if err := s.clearEndedBlock(); err != nil {
		return archive.SourceBundle{}, nil, err
	}
	cached, _, _, haveCached := s.published.Cached()
	lastPublished, _, _ := s.published.LastPublished()
	baseEvidence := lastPublished.SupplementalEvidence
	if haveCached {
		baseEvidence = cached.SupplementalEvidence
	}
	supplemental := mergeSupplementalEvidence(baseEvidence, s.req.HookEvidence)

	// now is a placeholder here; bundleEvidenceEqual ignores CapturedAt, so
	// it has no effect on the comparison. The real value is assigned once it
	// is known whether this is genuinely new evidence.
	candidate, err := archive.NewSourceBundle(s.reg, read.adapter, read.filtered, s.now, supplemental)
	if err != nil {
		return archive.SourceBundle{}, nil, fmt.Errorf("build source bundle: %w", err)
	}

	// Observe the filesystem only with session activity. An unrelated skill edit
	// must not refresh every historical session or extend its retention lifetime.
	active := !haveCached || s.req.Token != "" || !nativeEvidenceExtends(cached, candidate) || !nativeEvidenceExtends(candidate, cached)
	if s.opts.SupplementalEvidence == nil || !active {
		return candidate, supplemental, nil
	}
	observed, err := s.opts.SupplementalEvidence(s.reg, s.now)
	if err != nil {
		return archive.SourceBundle{}, nil, fmt.Errorf("collect supplemental evidence: %w", err)
	}
	supplemental = mergeSupplementalEvidence(baseEvidence, observed, s.req.HookEvidence)
	candidate, err = archive.NewSourceBundle(s.reg, read.adapter, read.filtered, s.now, supplemental)
	if err != nil {
		return archive.SourceBundle{}, nil, fmt.Errorf("build observed source bundle: %w", err)
	}
	return candidate, supplemental, nil
}

// clearEndedBlock retires a recoverable block (a transcript that was missing)
// now that the source is readable again, and picks up any hook evidence the
// block held back.
func (s *sessionScan) clearEndedBlock() error {
	if _, blocked := s.published.Blocked(); !blocked {
		return nil
	}
	_, replayed, _, err := s.published.ClearRecoverableBlock(s.now)
	if err != nil {
		return err
	}
	if replayed {
		// Hook evidence acknowledged while the file was away is pending
		// again, under a fresh token: pick it up now so it publishes with
		// the recovery, and so the settled outcomes acknowledge the request
		// that actually carries it.
		if s.req, _, err = s.local.EnsureRequestToken(s.id()); err != nil {
			return fmt.Errorf("reload replayed request: %w", err)
		}
	}
	return nil
}

// compare decides whether candidate is new evidence, and sets its capture
// time. settled reports that it is not, in which case the scan is over:
// the request is acknowledged and, unless a gap stands, the scan signature
// recorded so the next pass can skip the session.
func (s *sessionScan) compare(read sourceRead, candidate *archive.SourceBundle) (settled bool, err error) {
	cached, _, status, haveCached := s.published.Cached()
	changed := true
	if haveCached {
		same, err := bundleEvidenceEqual(cached, *candidate)
		if err != nil {
			return false, fmt.Errorf("compare source bundles: %w", err)
		}
		changed = !same
	}
	switch {
	case !haveCached, changed:
		// Evidence not seen before, whether relative to the last publish or
		// to a still-pending rate-limited candidate: this is a new snapshot,
		// first observed now. A change that only adds or updates a child link
		// is the exception: it carries no new activity of this session's own,
		// so it keeps the capture time its evidence was actually observed at.
		// So does the same evidence re-filtered by a new filter or adapter
		// version.
		candidate.Capture.CapturedAt = s.now
		if haveCached && !cached.Capture.CapturedAt.IsZero() {
			linkOnly, err := bundleChangeIsLinkOnly(cached, *candidate)
			if err != nil {
				return false, fmt.Errorf("compare linked sessions: %w", err)
			}
			refiltered, err := s.refilteredUnchanged(read, cached, *candidate)
			if err != nil {
				return false, err
			}
			if linkOnly || refiltered {
				candidate.Capture.CapturedAt = cached.Capture.CapturedAt
			}
		}
		return false, nil
	case status == state.CacheStatusRateLimited:
		// Unchanged since the last withheld candidate: it is the same
		// pending snapshot, so reuse its already-assigned capture time
		// rather than manufacturing a new one on every retry.
		candidate.Capture.CapturedAt = cached.Capture.CapturedAt
		return false, nil
	}
	// Unchanged since the last actual publish, or since a policy decline:
	// nothing to do. A decline is reconsidered only by a genuine further
	// content change, never by time alone.
	if err := s.completeRequest("complete unchanged request"); err != nil {
		return true, err
	}
	if reason, blocked := s.published.Blocked(); status == state.CacheStatusBlocked && blocked {
		// Unchanged since a rewrite was recorded: still the same gap, at
		// this same transcript, which the next pass skips until it changes.
		s.gap = reason
		return true, s.recordBlockedSignature(reason, &read.observed)
	}
	return true, s.recordScanSignature(read.observed, *candidate)
}

// refilteredUnchanged reports whether candidate differs from cached only
// because a new filter or adapter version filtered the same evidence: the
// versions differ, the source is exactly as the last settled scan left it
// (when cached was built from it), and this scan brought no hook evidence.
// Filtered output can't tell that on its own, since a new filter changes
// what earlier records look like. Without a settled scan signature to
// compare with, the change counts as new evidence.
func (s *sessionScan) refilteredUnchanged(read sourceRead, cached, candidate archive.SourceBundle) (bool, error) {
	if cached.Capture.FilterVersion == candidate.Capture.FilterVersion && cached.Capture.AdapterVersion == candidate.Capture.AdapterVersion {
		return false, nil
	}
	if len(s.req.HookEvidence) > 0 {
		return false, nil
	}
	signature, found, err := s.local.LoadScanSignature(s.id())
	if err != nil {
		return false, fmt.Errorf("load scan signature: %w", err)
	}
	return found && signature.Blocked == "" && !signature.Failed && read.observed.matches(signature), nil
}

// guard checks that candidate still extends the evidence already retained.
// A transcript that does not is blocked (blocked reports it); a Cursor chat
// that does not is taken as it now is, with a rewrite gap, and the
// candidate is rebuilt to carry it.
func (s *sessionScan) guard(read sourceRead, candidate archive.SourceBundle, supplemental []archive.SupplementalEvidence) (_ archive.SourceBundle, blocked bool, err error) {
	// A blocked candidate is itself the rewritten evidence, so it must never
	// become the baseline: keep guarding against what was actually published.
	guardBundle, _, haveGuard := s.published.LastPublished()
	if cached, _, status, haveCached := s.published.Cached(); haveCached && status != state.CacheStatusBlocked {
		guardBundle, haveGuard = cached, true
	}
	if !haveGuard || nativeEvidenceExtends(guardBundle, candidate) {
		return candidate, false, nil
	}
	if s.reg.SourceKind != archive.SourceKindCursorSQLite {
		// Truncated, compacted, or rewritten: the retained snapshot is richer
		// than what the file now holds, and nothing the collector can do will
		// change that. Record the gap so later passes are no-ops until the
		// transcript changes again, rather than an error on every pass.
		_, err := s.block(state.BlockedReasonTranscriptRewritten, &candidate, &read.observed)
		return candidate, true, err
	}
	// Cursor rewrites finished messages in its database as a matter of course
	// (token counts filled in late, an edited prompt, a checkpoint restore),
	// so a chat that no longer extends what was published is the chat as it
	// now is, not a damaged copy of it, and blocking it would stop capturing
	// the chat for good. The new snapshot replaces the old one, and the
	// chat's one rewrite gap, which names no content, counts the replacement.
	rebuilt, err := archive.NewSourceBundle(s.reg, read.adapter, read.filtered, s.now, withCursorRewriteGap(supplemental, s.now))
	if err != nil {
		return candidate, false, fmt.Errorf("build rewritten source bundle: %w", err)
	}
	return rebuilt, false, nil
}

// publish renders candidate's publication and decides what happens to it:
// declined by policy, held back by the upload interval, or published now.
func (s *sessionScan) publish(read sourceRead, candidate archive.SourceBundle) (sessionOutcome, error) {
	rendered, err := renderPublication(candidate, s.reg, s.now, s.opts)
	if err != nil {
		return outcomeSkipped, err
	}
	_, lastPublishedAt, _ := s.published.LastPublished()
	if rendered.declined {
		// Nothing was ever actually published, so this candidate carries no
		// real publish history; a zero PublishedAt correctly signals that to
		// the rate-limit check once this decline is reconsidered by a later
		// content change, rather than a manufactured timestamp making a
		// genuinely-first publish look rate-limited.
		if err := s.published.Save(candidate, lastPublishedAt, state.CacheStatusDeclined); err != nil {
			return outcomeSkipped, fmt.Errorf("cache declined candidate: %w", err)
		}
		if err := s.completeRequest("complete declined request"); err != nil {
			return outcomeSkipped, err
		}
		return outcomeSkipped, s.recordScanSignature(read.observed, candidate)
	}

	readyAt := publicationReadyAt(s.now, lastPublishedAt, s.req, s.opts)
	pending := state.PendingPublication{
		Bundle: candidate, SourceKey: rendered.source.Key, MetadataKey: rendered.metadataKey,
		SourceSHA256: rendered.source.SHA256, SourceBytes: rendered.sourceBytes, MetadataBytes: rendered.metadata,
		RequestToken: s.req.Token, ReadyAt: readyAt,
	}
	if err := s.local.SavePending(s.id(), pending); err != nil {
		return outcomeSkipped, fmt.Errorf("persist pending publication: %w", err)
	}
	if readyAt.After(s.now) {
		if err := s.published.Save(candidate, lastPublishedAt, state.CacheStatusRateLimited); err != nil {
			return outcomeSkipped, fmt.Errorf("cache rate-limited candidate: %w", err)
		}
		s.readyAt = readyAt
		return outcomeRateLimited, nil
	}
	outcome, err := s.publishPending(pending)
	if err != nil || outcome != outcomePublished {
		return outcome, err
	}
	return outcome, s.recordScanSignature(read.observed, candidate)
}

// renderedPublication is a candidate bundle rendered for upload.
type renderedPublication struct {
	source      archive.SourceReference
	sourceBytes []byte
	metadataKey string
	metadata    []byte
	// declined means Options.RequireSkillUse turns the session down.
	declined bool
}

// renderPublication compresses candidate, derives its object keys, and
// builds its metadata document.
func renderPublication(candidate archive.SourceBundle, reg archive.SessionRegistration, now time.Time, opts Options) (renderedPublication, error) {
	compressed, err := archive.BuildCompressedSource(candidate)
	if err != nil {
		return renderedPublication{}, fmt.Errorf("compress source bundle: %w", err)
	}
	sourceKey, err := archive.SourceObjectKey(candidate, compressed.SHA256)
	if err != nil {
		return renderedPublication{}, fmt.Errorf("derive source key: %w", err)
	}
	metadataKey, err := archive.MetadataObjectKey(candidate.Capture.Harness.Name, candidate.ArchiveSessionID)
	if err != nil {
		return renderedPublication{}, fmt.Errorf("derive metadata key: %w", err)
	}
	source := archive.SourceReference{Key: sourceKey, SHA256: compressed.SHA256, CompressedBytes: len(compressed.Bytes)}
	metadata, buildErr := archive.BuildMetadata(candidate, opts.MachineID, reg.SessionStartedAt, now, source, archive.ParserInfo{Version: opts.parserVersion()})
	metadata.ApplyRegistrationProvenance(reg)
	if buildErr != nil && !archive.IsParseError(buildErr) {
		return renderedPublication{}, fmt.Errorf("derive metadata: %w", buildErr)
	}
	// A ParseError here still yields a minimal, safe-to-publish metadata
	// document with parser.status "failed", per the spec's failure table:
	// archive the filtered source and retry parsing later. A ParseError
	// also means metadata.SkillsUsed is necessarily empty (parsing never
	// got far enough to derive it), so RequireSkillUse must not read that
	// as "no skill use" and decline the session — that would silently and
	// permanently skip every session whose transcript fails to parse,
	// contradicting the comment above and the failure table it cites.
	if opts.RequireSkillUse && !archive.IsParseError(buildErr) && len(metadata.SkillsUsed) == 0 {
		return renderedPublication{declined: true}, nil
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return renderedPublication{}, fmt.Errorf("marshal metadata: %w", err)
	}
	return renderedPublication{source: source, sourceBytes: compressed.Bytes, metadataKey: metadataKey, metadata: encoded}, nil
}

// publicationReadyAt is when a new publication may be uploaded: now for an
// urgent request or a session not published within the upload interval,
// otherwise one interval after the last publication. A last publication
// stamped in the future was stamped by a wrong clock (an NTP step, a VM
// resuming): it counts as just now, so it defers this one by at most one
// interval rather than until that date comes round.
func publicationReadyAt(now, lastPublishedAt time.Time, req state.Request, opts Options) time.Time {
	intervalFrom := lastPublishedAt
	if intervalFrom.After(now) {
		intervalFrom = now
	}
	if !req.Urgent() && !intervalFrom.IsZero() && now.Sub(intervalFrom) < opts.minUploadInterval() {
		return intervalFrom.Add(opts.minUploadInterval())
	}
	return now
}

// completeRequest acknowledges the request this scan covered, if any.
func (s *sessionScan) completeRequest(what string) error {
	if s.req.Token == "" {
		return nil
	}
	if _, err := s.local.CompleteRequest(s.id(), s.req.Token); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}
