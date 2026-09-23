package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// Options configures one Run call. The caller is responsible for holding
// local.Lock(home) around Run; Run itself does not acquire it, so it stays
// simple to call directly from tests.
type Options struct {
	// ParserVersion identifies metadata derivation independently of source capture.
	ParserVersion string
	AcceptSession func(archive.SessionRegistration) bool
	// MachineID identifies this machine in published metadata. Required.
	MachineID string
	// Now returns the current time. Defaults to time.Now; tests override it
	// for deterministic timestamps and rate-limit behavior.
	Now func() time.Time
	// Retry controls storage retry/backoff. Zero value uses storage's default.
	Retry storage.RetryPolicy
	// MinUploadInterval bounds how often one session may be republished.
	// Defaults to three minutes, matching the spec's cadence.
	MinUploadInterval time.Duration
	// RequireSkillUse, when true, skips publishing a session that has no
	// detected skill use. The spec's default is to capture such sessions
	// anyway (to preserve comparison evidence), so the zero value (false)
	// matches that default rather than requiring every caller to opt in.
	RequireSkillUse bool
	// SupplementalEvidence observes non-transcript evidence such as the
	// installed skill inventory. The collector merges stable observations
	// without letting a new polling timestamp manufacture a new snapshot.
	SupplementalEvidence func(archive.SessionRegistration, time.Time) ([]archive.SupplementalEvidence, error)
	// MaxTranscriptBytes caps the transcript size the collector will read. A
	// larger transcript is recorded as a capture gap (CacheStatusBlocked with
	// BlockedReasonTranscriptTooLarge) rather than an error. Zero uses
	// DefaultMaxTranscriptBytes.
	MaxTranscriptBytes int64
}

// DefaultMaxTranscriptBytes is the transcript size ceiling when
// Options.MaxTranscriptBytes is zero.
// It is defined from archive.MaxRecordBytes so the two cannot drift: any
// record inside a transcript the collector accepts can be read.
const DefaultMaxTranscriptBytes int64 = archive.MaxRecordBytes

// recordLimit is the largest single transcript record the collector reads,
// archive.MaxRecordBytes; a variable only so a test can lower it.
var recordLimit int64 = archive.MaxRecordBytes

func (o Options) maxTranscriptBytes() int64 {
	if o.MaxTranscriptBytes > 0 {
		return o.MaxTranscriptBytes
	}
	return DefaultMaxTranscriptBytes
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) minUploadInterval() time.Duration {
	if o.MinUploadInterval > 0 {
		return o.MinUploadInterval
	}
	return 3 * time.Minute
}

// Result summarizes one Run call. It never includes transcript content.
type Result struct {
	Scanned   int
	Published []string
	Skipped   []string
	Errors    map[string]error
}

// Run performs one collector pass over every registered session: for each,
// read its transcript, filter it, and compare the result against the last
// published bundle. A session with no meaningful change is left untouched at
// no storage cost. A session with new evidence is published source-before-
// metadata, subject to MinUploadInterval. One session's failure does not
// stop the pass; it is recorded in Result.Errors and left retryable on the
// next call.
func Run(ctx context.Context, local *LocalStore, store storage.ObjectStore, opts Options) (Result, error) {
	if local == nil || store == nil {
		return Result{}, errors.New("local store and object store are required")
	}
	if opts.MachineID == "" {
		return Result{}, errors.New("machine ID is required")
	}
	now := opts.now()
	materializationIssues := materializeSubagentCandidates(local, opts)

	registrations, err := local.LoadRegistrations()
	if err != nil {
		return Result{}, fmt.Errorf("load registrations: %w", err)
	}
	requestsByID := map[string]Request{}
	if requests, err := local.LoadRequests(); err != nil {
		return Result{}, fmt.Errorf("load requests: %w", err)
	} else {
		for _, req := range requests {
			if req.Token == "" {
				var found bool
				req, found, err = local.ensureRequestToken(req.ArchiveSessionID)
				if err != nil {
					return Result{}, fmt.Errorf("upgrade pending request: %w", err)
				}
				if !found {
					// Acknowledged by a concurrent process between listing
					// and upgrade; nothing is pending for it any more.
					continue
				}
			}
			requestsByID[req.ArchiveSessionID] = req
		}
	}

	result := Result{Errors: materializationIssues}
	pending := 0
	for _, reg := range registrations {
		if opts.AcceptSession != nil && !opts.AcceptSession(reg) {
			continue
		}
		result.Scanned++
		req := requestsByID[reg.ArchiveSessionID]

		if req.Token == "" {
			unchanged, err := unchangedSinceLastScan(local, reg, opts)
			if err != nil {
				return result, fmt.Errorf("check transcript for changes: %w", err)
			}
			if unchanged {
				// Nothing to read, nothing to compare, nothing to journal.
				result.Skipped = append(result.Skipped, reg.ArchiveSessionID)
				continue
			}
		}

		if err := local.SetScanPending(reg.ArchiveSessionID, true); err != nil {
			return result, fmt.Errorf("journal pending scan: %w", err)
		}
		if err := markPublishedSubagent(local, reg); err != nil {
			result.Errors[reg.ArchiveSessionID] = err
			pending++
		}
		outcome, err := processSession(ctx, local, store, reg, req, now, opts)
		if err != nil {
			result.Errors[reg.ArchiveSessionID] = err
			pending++
			continue
		}
		_, requestPending, requestErr := local.loadRequest(reg.ArchiveSessionID)
		if requestErr != nil {
			return result, fmt.Errorf("check pending request: %w", requestErr)
		}
		_, uploadPending, uploadErr := local.LoadPending(reg.ArchiveSessionID)
		if uploadErr != nil {
			return result, fmt.Errorf("check pending publication: %w", uploadErr)
		}
		if !requestPending && !uploadPending {
			if err := local.SetScanPending(reg.ArchiveSessionID, false); err != nil {
				return result, fmt.Errorf("complete pending scan: %w", err)
			}
		} else {
			pending++
		}
		switch outcome {
		case outcomePublished:
			result.Published = append(result.Published, reg.ArchiveSessionID)
			if err := markPublishedSubagent(local, reg); err != nil {
				result.Errors[reg.ArchiveSessionID] = err
				pending++
			}
		case outcomeRateLimited:
			result.Skipped = append(result.Skipped, reg.ArchiveSessionID)
		case outcomeSkipped:
			result.Skipped = append(result.Skipped, reg.ArchiveSessionID)
		}
	}

	previousStatus, err := local.LoadStatus()
	if err != nil {
		return result, err
	}
	status := Status{LastScanAt: now.UTC(), PendingCount: pending, LastPublishedAt: previousStatus.LastPublishedAt}
	if len(result.Published) > 0 {
		status.LastPublishedAt = now.UTC()
	}
	if len(result.Errors) > 0 {
		status.LastError = fmt.Sprintf("%d session(s) failed to scan or publish", len(result.Errors))
	}
	if err := local.SaveStatus(status); err != nil {
		return result, fmt.Errorf("save status: %w", err)
	}
	return result, nil
}

// cursorTextSourceFormat labels a Cursor transcript captured as plain text.
const cursorTextSourceFormat = "cursor-text"

// unchangedSinceLastScan answers the spec's "transcript changed?" question
// without opening, reading, parsing, or journaling anything: one stat of the
// transcript plus two tiny local files. It is the difference between a pass
// costing time proportional to every session this machine has ever registered
// and one costing time proportional to the sessions that actually moved.
//
// It says yes only when all of the following hold, because each of them is a
// way an unchanged file can still owe work:
//
//   - The last completed scan left a signature (see scanSignature). A blocked
//     session has none, so a capture gap is always re-evaluated.
//   - The transcript is a regular file whose size and nanosecond modification
//     time both still match that signature.
//   - The parser, filter, and adapter versions still match, so an upgrade
//     re-derives every session instead of skipping it.
//   - No publication is pending and no interrupted scan is journaled.
//   - The session is not a subagent, whose publication also notifies a parent.
//
// The caller has already established that no hook request is pending.
//
// The residual risk is a transcript rewritten in place to exactly its previous
// byte length within the same nanosecond. Nanosecond mtimes make that
// essentially unreachable for a real application, but it is not a proof, so a
// Cursor text transcript — which has no per-record timestamps and is compared
// by prefix, making a silent in-place edit hardest to detect downstream — is
// never skipped on a stat alone.
func unchangedSinceLastScan(local *LocalStore, reg archive.SessionRegistration, opts Options) (bool, error) {
	if reg.TranscriptPath == "" || reg.ParentSessionID != "" {
		return false, nil
	}
	signature, found, err := local.loadScanSignature(reg.ArchiveSessionID)
	if err != nil || !found {
		return false, err
	}
	if signature.SourceFormat == cursorTextSourceFormat {
		return false, nil
	}
	adapter, err := archive.NewAdapter(reg.Harness.Name)
	if err != nil {
		return false, nil
	}
	if signature.ParserVersion != opts.parserVersion() || signature.FilterVersion != archive.FilterVersion || signature.AdapterVersion != adapter.Version() {
		return false, nil
	}
	info, err := os.Stat(reg.TranscriptPath)
	if err != nil || !info.Mode().IsRegular() {
		return false, nil
	}
	if statTranscript(info) != (transcriptFileInfo{Size: signature.TranscriptSize, Mtime: signature.TranscriptMtime}) {
		return false, nil
	}
	if scanPending, err := local.ScanPending(reg.ArchiveSessionID); err != nil || scanPending {
		return false, err
	}
	pending, err := local.HasPending(reg.ArchiveSessionID)
	if err != nil || pending {
		return false, err
	}
	return true, nil
}

// recordScanSignature marks a session settled at the transcript bytes this
// scan consumed, so the next pass can skip it. It is written only at an exit
// that owes no further work.
func recordScanSignature(local *LocalStore, reg archive.SessionRegistration, stat transcriptFileInfo, bundle archive.SourceBundle, opts Options) error {
	return local.saveScanSignature(reg.ArchiveSessionID, scanSignature{
		TranscriptSize: stat.Size, TranscriptMtime: stat.Mtime,
		ParserVersion: opts.parserVersion(), FilterVersion: bundle.Capture.FilterVersion,
		AdapterVersion: bundle.Capture.AdapterVersion, SourceFormat: bundle.Capture.SourceFormat,
	})
}

type sessionOutcome int

const (
	outcomeSkipped sessionOutcome = iota
	outcomeRateLimited
	outcomePublished
)

func processSession(ctx context.Context, local *LocalStore, store storage.ObjectStore, reg archive.SessionRegistration, req Request, now time.Time, opts Options) (sessionOutcome, error) {
	// A publication that may already have reached storage is immutable local
	// work. Retry its exact bytes before considering later transcript changes.
	pending, havePending, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil {
		return outcomeSkipped, err
	}
	if havePending {
		newRequest := !pending.Attempted && req.Token != "" && req.Token != pending.RequestToken
		if !newRequest {
			if !pending.ReadyAt.IsZero() && now.Before(pending.ReadyAt) {
				return outcomeRateLimited, nil
			}
			return publishPending(ctx, local, store, reg.ArchiveSessionID, pending, now, opts)
		}
		// A stop/end request is a natural debounce flush. A merely rate-limited,
		// never-attempted candidate can be safely replaced by a richer one;
		// deferred lifecycle evidence enriches it without changing when it
		// becomes ready.
	}
	if !havePending {
		if outcome, handled, err := regenerateMetadata(ctx, local, store, reg, now, opts); handled || err != nil {
			return outcome, err
		}
	}

	if reg.TranscriptPath == "" {
		// Not an error: a Cursor desktop chat is registered at its first
		// prompt, before Cursor names its transcript, and a later hook fills
		// the path in. Until then there is nothing to read. The session is
		// retried every pass (unchangedSinceLastScan never skips it), any
		// hook request stays queued, and nothing is recorded as a failure.
		return outcomeSkipped, nil
	}

	adapter, err := archive.NewAdapter(reg.Harness.Name)
	if err != nil {
		return outcomeSkipped, err
	}
	filtered, transcriptStat, err := filterTranscript(adapter, reg, opts.maxTranscriptBytes())
	if err != nil {
		if errors.Is(err, errTranscriptTooLarge) {
			// The file will not shrink by retrying: record the gap once and
			// keep the last published snapshot instead of failing every pass.
			return blockSession(local, reg.ArchiveSessionID, req, BlockedReasonTranscriptTooLarge, nil)
		}
		if errors.Is(err, errRecordTooLarge) {
			// Likewise one record too long to read: a gap recorded once, the
			// last published snapshot kept, cleared when the file changes.
			return blockSession(local, reg.ArchiveSessionID, req, BlockedReasonRecordTooLarge, nil)
		}
		if errors.Is(err, os.ErrNotExist) {
			// The application deleted its own transcript. Retention keeps
			// sessions for far longer than any of them keep their logs, so
			// this is the ordinary end state of every archived session, not a
			// failure: record it as a capture gap once, keep the last
			// published snapshot, and let a returning file clear it.
			return blockSession(local, reg.ArchiveSessionID, req, BlockedReasonTranscriptMissing, nil)
		}
		// Unsafe format: never upload; the last published snapshot, if any,
		// remains untouched and readable.
		return outcomeSkipped, fmt.Errorf("filter transcript: %w", err)
	}
	if transcriptStat.Size == 0 {
		// The application has created the file but written no record yet:
		// the fresh-start proof accepts exactly this state, so it is the
		// same waiting as having no path at all, one step later. Nothing has
		// been captured, so there is nothing to block or fail; the request
		// stays queued and the next pass reads whatever has arrived. A file
		// emptied after a publication is a rewrite, which the comparison
		// below still reports rather than waits on.
		if _, _, published, err := local.LoadLastPublished(reg.ArchiveSessionID); err != nil {
			return outcomeSkipped, fmt.Errorf("load last published bundle: %w", err)
		} else if !published {
			return outcomeSkipped, nil
		}
	}
	if err := validateSubagentTranscript(reg, filtered); err != nil {
		return outcomeSkipped, err
	}

	prevBundle, _, prevStatus, havePrev, err := local.LoadPublished(reg.ArchiveSessionID)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("load published cache: %w", err)
	}
	if havePrev && prevStatus == CacheStatusBlocked {
		// The transcript is readable again. A file that comes back carrying
		// exactly what was already captured produces no change for the
		// comparison below to act on, so the gap has to be retired here or it
		// would be reported for the rest of the session's life.
		restored, replayed, cleared, err := local.ClearRecoverableBlock(reg.ArchiveSessionID, now)
		if err != nil {
			return outcomeSkipped, err
		}
		if cleared {
			prevStatus = restored
		}
		if replayed {
			// Hook evidence acknowledged while the file was away is pending
			// again, under a fresh token: pick it up now so it publishes with
			// the recovery, and so the settled outcomes below acknowledge the
			// request that actually carries it.
			if req, _, err = local.ensureRequestToken(reg.ArchiveSessionID); err != nil {
				return outcomeSkipped, fmt.Errorf("reload replayed request: %w", err)
			}
		}
	}

	lastPublished, lastPublishedAt, haveLastPublished, err := local.LoadLastPublished(reg.ArchiveSessionID)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("load last published bundle: %w", err)
	}
	baseEvidence := lastPublished.SupplementalEvidence
	if havePrev {
		baseEvidence = prevBundle.SupplementalEvidence
	}
	supplemental := mergeSupplementalEvidence(baseEvidence, req.HookEvidence)

	// now is a placeholder here; bundleEvidenceEqual ignores CapturedAt, so
	// it has no effect on the comparison below. The real value is assigned
	// once we know whether this is genuinely new evidence.
	candidate, err := archive.NewSourceBundle(reg, adapter, filtered, now, supplemental)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("build source bundle: %w", err)
	}

	// Observe the filesystem only with session activity. An unrelated skill edit
	// must not refresh every historical session or extend its retention lifetime.
	if opts.SupplementalEvidence != nil && (!havePrev || req.Token != "" || !nativeEvidenceExtends(prevBundle, candidate) || !nativeEvidenceExtends(candidate, prevBundle)) {
		observed, err := opts.SupplementalEvidence(reg, now)
		if err != nil {
			return outcomeSkipped, fmt.Errorf("collect supplemental evidence: %w", err)
		}
		supplemental = mergeSupplementalEvidence(baseEvidence, observed, req.HookEvidence)
		candidate, err = archive.NewSourceBundle(reg, adapter, filtered, now, supplemental)
		if err != nil {
			return outcomeSkipped, fmt.Errorf("build observed source bundle: %w", err)
		}
	}

	changedFromCache := true
	if havePrev {
		same, err := bundleEvidenceEqual(prevBundle, candidate)
		if err != nil {
			return outcomeSkipped, fmt.Errorf("compare source bundles: %w", err)
		}
		changedFromCache = !same
	}

	switch {
	case !havePrev, changedFromCache:
		// Evidence not seen before, whether relative to the last publish or
		// to a still-pending rate-limited candidate: this is a new snapshot,
		// first observed now. A change that only adds or updates a child link
		// is the exception: it carries no new activity of this session's own,
		// so it keeps the capture time its evidence was actually observed at.
		candidate.Capture.CapturedAt = now
		if havePrev && !prevBundle.Capture.CapturedAt.IsZero() {
			linkOnly, err := bundleChangeIsLinkOnly(prevBundle, candidate)
			if err != nil {
				return outcomeSkipped, fmt.Errorf("compare linked sessions: %w", err)
			}
			if linkOnly {
				candidate.Capture.CapturedAt = prevBundle.Capture.CapturedAt
			}
		}
	case prevStatus == CacheStatusRateLimited:
		// Unchanged since the last withheld candidate: it is the same
		// pending snapshot, so reuse its already-assigned capture time
		// rather than manufacturing a new one on every retry.
		candidate.Capture.CapturedAt = prevBundle.Capture.CapturedAt
	default:
		// Unchanged since the last actual publish, or since a policy
		// decline: nothing to do. A decline is reconsidered only by a
		// genuine further content change, never by time alone.
		if req.Token != "" {
			if _, err := local.CompleteRequest(reg.ArchiveSessionID, req.Token); err != nil {
				return outcomeSkipped, fmt.Errorf("complete unchanged request: %w", err)
			}
		}
		if prevStatus == CacheStatusBlocked {
			// Unchanged since a rewrite was recorded: still a gap, and a gap
			// is re-evaluated on every pass rather than skipped on a stat.
			return outcomeSkipped, nil
		}
		return outcomeSkipped, recordScanSignature(local, reg, transcriptStat, candidate, opts)
	}

	// A blocked candidate is itself the rewritten evidence, so it must never
	// become the baseline: keep guarding against what was actually published.
	guardBundle, haveGuard := lastPublished, haveLastPublished
	if havePrev && prevStatus != CacheStatusBlocked {
		guardBundle, haveGuard = prevBundle, true
	}
	if haveGuard && !nativeEvidenceExtends(guardBundle, candidate) {
		// Truncated, compacted, or rewritten: the retained snapshot is richer
		// than what the file now holds, and nothing the collector can do will
		// change that. Record the gap so later passes are no-ops until the
		// transcript changes again, rather than an error on every pass.
		return blockSession(local, reg.ArchiveSessionID, req, BlockedReasonTranscriptRewritten, &candidate)
	}

	compressed, err := archive.BuildCompressedSource(candidate)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("compress source bundle: %w", err)
	}
	sourceKey, err := archive.SourceObjectKey(candidate, compressed.SHA256)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("derive source key: %w", err)
	}
	metadataKey, err := archive.MetadataObjectKey(candidate.Capture.Harness.Name, candidate.ArchiveSessionID)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("derive metadata key: %w", err)
	}

	reference := archive.SourceReference{Key: sourceKey, SHA256: compressed.SHA256, CompressedBytes: len(compressed.Bytes)}
	metadata, buildErr := archive.BuildMetadata(candidate, opts.MachineID, reg.SessionStartedAt, now, reference, archive.ParserInfo{Version: opts.parserVersion()})
	if buildErr != nil && !archive.IsParseError(buildErr) {
		return outcomeSkipped, fmt.Errorf("derive metadata: %w", buildErr)
	}
	// A ParseError here still yields a minimal, safe-to-publish metadata
	// document with parser.status "failed", per the spec's failure table:
	// archive the filtered source and retry parsing later. A ParseError
	// also means metadata.SkillsUsed is necessarily empty (parsing never
	// got far enough to derive it), so RequireSkillUse must not read that
	// as "no skill use" and decline the session — that would silently and
	// permanently skip every session whose transcript fails to parse,
	// contradicting the comment above and the failure table it cites.
	parseFailed := archive.IsParseError(buildErr)

	if opts.RequireSkillUse && !parseFailed && len(metadata.SkillsUsed) == 0 {
		// Nothing was ever actually published, so this candidate carries no
		// real publish history; a zero PublishedAt correctly signals that
		// to the rate-limit check above once this decline is reconsidered
		// by a later content change, rather than a manufactured timestamp
		// making a genuinely-first publish look rate-limited.
		if err := local.SavePublished(reg.ArchiveSessionID, candidate, lastPublishedAt, CacheStatusDeclined); err != nil {
			return outcomeSkipped, fmt.Errorf("cache declined candidate: %w", err)
		}
		if req.Token != "" {
			if _, err := local.CompleteRequest(reg.ArchiveSessionID, req.Token); err != nil {
				return outcomeSkipped, fmt.Errorf("complete declined request: %w", err)
			}
		}
		return outcomeSkipped, recordScanSignature(local, reg, transcriptStat, candidate, opts)
	}

	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("marshal metadata: %w", err)
	}

	readyAt := now
	if !req.urgent() && !lastPublishedAt.IsZero() && now.Sub(lastPublishedAt) < opts.minUploadInterval() {
		readyAt = lastPublishedAt.Add(opts.minUploadInterval())
	}
	pending = PendingPublication{
		Bundle: candidate, SourceKey: sourceKey, MetadataKey: metadataKey,
		SourceSHA256: compressed.SHA256, SourceBytes: compressed.Bytes, MetadataBytes: metadataBytes,
		RequestToken: req.Token, ReadyAt: readyAt,
	}
	if err := local.SavePending(reg.ArchiveSessionID, pending); err != nil {
		return outcomeSkipped, fmt.Errorf("persist pending publication: %w", err)
	}
	if readyAt.After(now) {
		if err := local.SavePublished(reg.ArchiveSessionID, candidate, lastPublishedAt, CacheStatusRateLimited); err != nil {
			return outcomeSkipped, fmt.Errorf("cache rate-limited candidate: %w", err)
		}
		return outcomeRateLimited, nil
	}
	outcome, err := publishPending(ctx, local, store, reg.ArchiveSessionID, pending, now, opts)
	if err != nil || outcome != outcomePublished {
		return outcome, err
	}
	return outcome, recordScanSignature(local, reg, transcriptStat, candidate, opts)
}

// blockSession records a terminal capture gap for one session and completes
// its request, so the pass ends cleanly and the session is not counted as
// pending. candidate, when known, becomes the cached comparison bundle so an
// unchanged transcript is skipped on the next pass; otherwise the previously
// cached bundle (or the last published one) is kept. The last published
// snapshot is retained untouched either way.
func blockSession(local *LocalStore, id string, req Request, reason BlockedReason, candidate *archive.SourceBundle) (sessionOutcome, error) {
	prevBundle, _, prevStatus, havePrev, err := local.LoadPublished(id)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("load published cache: %w", err)
	}
	lastPublished, lastPublishedAt, _, err := local.LoadLastPublished(id)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("load last published bundle: %w", err)
	}
	bundle := lastPublished
	if havePrev {
		bundle = prevBundle
	}
	if candidate != nil {
		bundle = *candidate
	}
	alreadyBlocked := false
	if havePrev && prevStatus == CacheStatusBlocked {
		prevReason, _, err := local.LoadBlocked(id)
		if err != nil {
			return outcomeSkipped, err
		}
		alreadyBlocked = prevReason == reason && candidate == nil
	}
	// A block that can end holds on to the hook evidence its acknowledgement
	// would otherwise discard, so a final response that arrived just before
	// the application deleted its transcript still publishes if the file
	// comes back. A permanent block has nothing to hand it back to.
	var held []archive.SupplementalEvidence
	if reason.recoverable() {
		held = req.HookEvidence
	}
	if !alreadyBlocked || len(held) > 0 {
		if err := local.SaveBlocked(id, bundle, lastPublishedAt, reason, held...); err != nil {
			return outcomeSkipped, fmt.Errorf("cache blocked session: %w", err)
		}
	}
	// A recorded gap is re-evaluated on every pass, never skipped on a stat:
	// the condition that caused it (a deleted file above all) can end without
	// the transcript itself changing.
	if err := local.removeScanSignature(id); err != nil {
		return outcomeSkipped, err
	}
	if req.Token != "" {
		if _, err := local.CompleteRequest(id, req.Token); err != nil {
			return outcomeSkipped, fmt.Errorf("complete blocked request: %w", err)
		}
	}
	return outcomeSkipped, nil
}

func publishPending(ctx context.Context, local *LocalStore, store storage.ObjectStore, id string, pending PendingPublication, now time.Time, opts Options) (sessionOutcome, error) {
	if !storage.VerifySHA256(pending.SourceBytes, pending.SourceSHA256) {
		return outcomeSkipped, errors.New("pending source checksum does not match its persisted bytes")
	}
	pending.Attempted = true
	if err := local.SavePending(id, pending); err != nil {
		return outcomeSkipped, fmt.Errorf("mark pending publication attempted: %w", err)
	}
	if err := storage.PutSourceThenMetadata(ctx, store, pending.SourceKey, pending.MetadataKey, pending.SourceBytes, pending.MetadataBytes, opts.Retry); err != nil {
		return outcomeSkipped, fmt.Errorf("publish: %w", err)
	}
	previous, _, hadPrevious, err := local.LoadLastPublished(id)
	if err != nil {
		return outcomeSkipped, err
	}
	if hadPrevious {
		compressed, err := archive.BuildCompressedSource(previous)
		if err != nil {
			return outcomeSkipped, fmt.Errorf("rebuild previous source reference: %w", err)
		}
		previousKey, err := archive.SourceObjectKey(previous, compressed.SHA256)
		if err != nil {
			return outcomeSkipped, fmt.Errorf("rebuild previous source reference: %w", err)
		}
		if previousKey != pending.SourceKey {
			if err := local.RecordSuperseded(id, previousKey, now); err != nil {
				return outcomeSkipped, fmt.Errorf("record superseded source: %w", err)
			}
		}
	}
	var saveErr error
	if pending.MetadataOnly {
		saveErr = local.saveRepublishedMetadata(id, pending, now)
	} else {
		saveErr = local.SavePublished(id, pending.Bundle, now, CacheStatusPublished, pending.MetadataBytes)
	}
	if err := saveErr; err != nil {
		return outcomeSkipped, fmt.Errorf("update published cache: %w", err)
	}
	if pending.RequestToken != "" {
		if _, err := local.CompleteRequest(id, pending.RequestToken); err != nil {
			return outcomeSkipped, fmt.Errorf("complete published request: %w", err)
		}
	}
	if err := local.RemovePending(id); err != nil {
		return outcomeSkipped, err
	}
	return outcomePublished, nil
}

// filterTranscript filters reg's transcript with adapter, falling back to
// CursorAdapter's text format when Cursor's own JSONL filter reports the
// content isn't recognized as JSONL at all: the spec notes Cursor's
// hook-provided transcript path can point to either format depending on
// version, and the collector has no other way to tell which one it has
// until it tries. Any other adapter, or any other kind of filter failure,
// is returned as-is with no retry. A transcript larger than maxBytes yields
// an error wrapping errTranscriptTooLarge.
var errTranscriptTooLarge = errors.New("transcript exceeds collection limit")

// errRecordTooLarge means one record of the transcript is longer than
// recordLimit. With the defaults it cannot occur, since a transcript over the
// same limit is refused whole first; it can when Options.MaxTranscriptBytes
// is raised above archive.MaxRecordBytes.
var errRecordTooLarge = errors.New("transcript record exceeds the record size limit")

// transcriptFileInfo is the identity of the exact file bytes one scan read:
// its size and its modification time at nanosecond resolution. Second
// resolution would not be enough, because an application can rewrite a
// transcript in place within the same second; see unchangedSinceLastScan.
type transcriptFileInfo struct {
	Size  int64
	Mtime int64
}

func statTranscript(info os.FileInfo) transcriptFileInfo {
	return transcriptFileInfo{Size: info.Size(), Mtime: info.ModTime().UnixNano()}
}

func filterTranscript(adapter archive.Adapter, reg archive.SessionRegistration, maxBytes int64) (archive.FilteredTranscript, transcriptFileInfo, error) {
	file, err := os.Open(reg.TranscriptPath)
	if err != nil {
		return archive.FilteredTranscript{}, transcriptFileInfo{}, fmt.Errorf("open transcript: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return archive.FilteredTranscript{}, transcriptFileInfo{}, fmt.Errorf("stat transcript: %w", err)
	}
	// Taken before any read, so it describes the bytes this scan is about to
	// consume. A write that lands afterwards moves the mtime and is therefore
	// seen as a change by the next pass, which is the conservative direction.
	stat := statTranscript(info)
	boundary := info.Size()
	if boundary > maxBytes {
		return archive.FilteredTranscript{}, stat, fmt.Errorf("%w of %d bytes", errTranscriptTooLarge, maxBytes)
	}
	if boundary < 0 {
		return archive.FilteredTranscript{}, stat, errors.New("transcript has invalid size")
	}
	jsonBoundary, err := completeJSONLBoundary(file, boundary, recordLimit)
	if errors.Is(err, errRecordTooLarge) {
		return archive.FilteredTranscript{}, stat, err
	}
	if err != nil {
		return archive.FilteredTranscript{}, stat, fmt.Errorf("find complete transcript boundary: %w", err)
	}
	limited := &recordLimitReader{r: io.NewSectionReader(file, 0, jsonBoundary), limit: recordLimit}
	filtered, err := adapter.FilterJSONL(limited)
	if err == nil {
		return filtered, stat, nil
	}
	if limited.exceeded || errors.Is(err, archive.ErrRecordTooLarge) {
		return archive.FilteredTranscript{}, stat, errRecordTooLarge
	}
	cursorAdapter, ok := adapter.(archive.CursorAdapter)
	if !ok || !errors.Is(err, archive.ErrUnsafeSourceFormat) {
		return archive.FilteredTranscript{}, stat, err
	}
	filtered, err = cursorAdapter.FilterText(io.NewSectionReader(file, 0, boundary), reg.SessionStartedAt)
	return filtered, stat, err
}

// boundaryChunk is how much of the transcript completeJSONLBoundary reads at a
// time while it looks backward for the last newline.
const boundaryChunk = 64 * 1024

// completeJSONLBoundary returns the offset just past the transcript's last
// complete record, so a final record the harness is still writing is ignored.
// size was fixed by the caller before this check.
//
// A transcript that ends in a newline, the ordinary case, costs a one-byte
// read. Otherwise it scans backward from the end in boundaryChunk pieces for
// the last newline, never further back than limit+1 bytes: a trailing record
// that long cannot be read, and errRecordTooLarge says so. The trailing bytes
// after that newline are a complete record when they are valid JSON (a final
// record written without its newline); otherwise they are still being written
// and the boundary is the newline. A transcript with no newline at all is
// taken whole, as before.
func completeJSONLBoundary(file io.ReaderAt, size, limit int64) (int64, error) {
	if size <= 0 {
		return 0, nil
	}
	var last [1]byte
	if _, err := file.ReadAt(last[:], size-1); err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	if last[0] == '\n' {
		return size, nil
	}
	// The earliest offset a newline may have for the record after it to fit.
	floor := max(0, size-limit-1)
	newline := int64(-1)
	chunk := make([]byte, boundaryChunk)
	for end := size; end > floor && newline < 0; {
		start := max(floor, end-boundaryChunk)
		buf := chunk[:end-start]
		if _, err := file.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			newline = start + int64(i)
		}
		end = start
	}
	if newline < 0 {
		if size > limit {
			return 0, errRecordTooLarge
		}
		return size, nil
	}
	trailing := make([]byte, size-newline-1)
	if _, err := file.ReadAt(trailing, newline+1); err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	if json.Valid(bytes.TrimSpace(trailing)) {
		return size, nil
	}
	return newline + 1, nil
}

// recordLimitReader passes a transcript through unchanged while checking that
// no line is longer than limit. The collector sets the limit, not the adapter,
// so the policy (block the session with a capture gap) stays with the
// collector; exceeded tells the caller that a failed filter was this. It adds
// one byte scan over data the filter reads anyway, and no allocation.
type recordLimitReader struct {
	r        io.Reader
	limit    int64
	line     int64
	exceeded bool
}

func (l *recordLimitReader) Read(p []byte) (int, error) {
	if l.exceeded {
		return 0, errRecordTooLarge
	}
	n, err := l.r.Read(p)
	for data := p[:n]; len(data) > 0; {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			l.line += int64(len(data))
			break
		}
		if l.line+int64(i) > l.limit {
			l.exceeded = true
			return 0, errRecordTooLarge
		}
		l.line, data = 0, data[i+1:]
	}
	if l.line > l.limit {
		l.exceeded = true
		return 0, errRecordTooLarge
	}
	return n, err
}

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
