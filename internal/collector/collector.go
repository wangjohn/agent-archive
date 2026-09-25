// Package collector runs the local scan/build/publish loop that turns a
// hook-registered session into a published metadata sidecar and source
// bundle. It owns publication cadence and change detection; it keeps its
// local state through internal/state, builds on internal/local for atomic
// file I/O and the machine-level lock, and does not own transcript reading
// (archive adapters), privacy filtering (archive adapters), or storage
// upload mechanics (storage.PutSourceThenMetadata). A caller runs Run under
// local.Lock(home) so only one collector process acts on a given home at a
// time; Run itself does not take that lock.
//
// A pass (Run) lists the registered sessions and scans each in turn; one
// scan of one session is a sessionScan, whose steps and cache-status
// transitions are described in session.go.
package collector

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/state"
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
	// Progress, when set, is called after each session the pass processes,
	// with whether it published. Backfill draws its upload progress from it.
	Progress func(Progress)
	// Stop, when set, is checked before each session; once it reports true
	// the pass ends early. The session in flight always finishes, and the
	// rest keep their pending work for the next pass.
	Stop func() bool
	// CursorDatabase is Cursor's state.vscdb, which cursor-sqlite sessions
	// are read from. Empty means the one under the user's home directory.
	CursorDatabase string

	// cursorPass is the pass's Reader of Cursor's database, set by Run.
	cursorPass *cursorstore.Reader
}

// Progress reports one session a pass has processed.
type Progress struct {
	ArchiveSessionID string
	Published        bool
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
	// Skipped lists sessions with nothing new to publish.
	Skipped []string
	// Waiting lists sessions whose new evidence is saved and held for the
	// upload interval (Options.MinUploadInterval); NextReadyAt is when the
	// earliest of them becomes due.
	Waiting     []string
	NextReadyAt time.Time
	Errors      map[string]error
}

// Run performs one collector pass over every registered session: for each,
// read its transcript, filter it, and compare the result against the last
// published bundle. A session with no meaningful change is left untouched at
// no storage cost. A session with new evidence is published source-before-
// metadata, subject to MinUploadInterval. One session's failure does not
// stop the pass; it is recorded in Result.Errors and left retryable on the
// next call.
func Run(ctx context.Context, local *state.Store, store storage.ObjectStore, opts Options) (_ Result, runErr error) {
	if local == nil || store == nil {
		return Result{}, errors.New("local store and object store are required")
	}
	if opts.MachineID == "" {
		return Result{}, errors.New("machine ID is required")
	}
	now := opts.now()
	// The caller holds the collector lock, so this pass is the only writer
	// of the files it owns and may move a corrupt one aside.
	local = local.ForCollectorPass()
	local.RemoveStaleTemps()
	p := &pass{
		ctx:    ctx,
		local:  local,
		remote: store,
		opts:   opts,
		now:    now,
		result: Result{Errors: materializeSubagentCandidates(local, opts)},
	}
	if err := p.loadWork(); err != nil {
		return Result{}, err
	}
	orderOldestRequestsFirst(p.registrations, p.requests)
	closeCursorPass := openCursorPass(p.registrations, &p.opts)
	defer func() {
		if err := closeCursorPass(); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}()
	for i, reg := range p.registrations {
		// A pass past its deadline ends like a stopped one: nothing more can
		// reach storage, so the rest keep their work rather than each
		// failing on the expired context.
		if (p.opts.Stop != nil && p.opts.Stop()) || ctx.Err() != nil {
			p.leaveForNextPass(p.registrations[i:])
			break
		}
		if p.opts.AcceptSession != nil && !p.opts.AcceptSession(reg) {
			continue
		}
		p.scan(reg)
	}
	return p.result, p.saveStatus()
}

// pass is one Run: its inputs, what it has found so far, and its result.
type pass struct {
	ctx    context.Context
	local  *state.Store
	remote storage.ObjectStore
	opts   Options
	now    time.Time

	registrations []archive.SessionRegistration
	requests      map[string]state.Request
	// unreadable holds the sessions this pass must leave alone: their state
	// could not be read, so there is nothing safe to act on.
	unreadable map[string]bool
	// pending counts the sessions left with outstanding work.
	pending int
	result  Result
}

// loadWork lists the registered sessions and their pending requests. One
// unreadable state file fails only its own session: the pass, and the
// retention sweep after it, go on for every other one.
func (p *pass) loadWork() error {
	registrations, registrationIssues, err := p.local.ScanRegistrations()
	if err != nil {
		return err
	}
	requests, requestIssues, err := p.local.ScanRequests()
	if err != nil {
		return err
	}
	p.registrations, p.requests, p.unreadable = registrations, map[string]state.Request{}, map[string]bool{}
	// A session whose registration could not be read is left for the next
	// pass like any other outstanding work.
	p.pending = len(registrationIssues)
	registered := make(map[string]bool, len(registrations)+len(registrationIssues))
	for _, reg := range registrations {
		registered[reg.ArchiveSessionID] = true
	}
	for id, issue := range registrationIssues {
		addError(p.result.Errors, id, issue)
		registered[id] = !errors.Is(issue, state.ErrQuarantined)
	}
	// Lock files of sessions and candidates that are gone go now, while the
	// registrations just listed say which those are.
	p.local.RemoveOrphanedLocks(registered)
	for id, issue := range requestIssues {
		addError(p.result.Errors, id, issue)
		// A quarantined request is gone, and the session carries on without
		// it; one that could not be read may still hold evidence.
		if !errors.Is(issue, state.ErrQuarantined) {
			p.unreadable[id] = true
		}
	}
	for _, req := range requests {
		if req.Token == "" {
			id := req.ArchiveSessionID
			var found bool
			req, found, err = p.local.EnsureRequestToken(id)
			if err != nil {
				addError(p.result.Errors, id, fmt.Errorf("upgrade pending request: %w", err))
				p.unreadable[id] = true
				continue
			}
			if !found {
				// Acknowledged by a concurrent process between listing and
				// upgrade; nothing is pending for it any more.
				continue
			}
		}
		p.requests[req.ArchiveSessionID] = req
	}
	return nil
}

// leaveForNextPass counts the sessions an early end did not reach whose
// work (a request) is left for the next pass.
func (p *pass) leaveForNextPass(rest []archive.SessionRegistration) {
	for _, reg := range rest {
		if p.requests[reg.ArchiveSessionID].Token != "" && (p.opts.AcceptSession == nil || p.opts.AcceptSession(reg)) {
			p.pending++
		}
	}
}

// fail records a local failure that ends a session's turn in the pass; the
// session is retried on the next one.
func (p *pass) fail(id string, err error) {
	addError(p.result.Errors, id, err)
	p.pending++
	p.opts.progress(id, false)
}

// scan gives one session its turn in the pass: skip it if nothing about it
// changed, otherwise scan it (processSession) and account for the outcome.
func (p *pass) scan(reg archive.SessionRegistration) {
	id := reg.ArchiveSessionID
	p.result.Scanned++
	if p.unreadable[id] {
		p.pending++
		p.opts.progress(id, false)
		return
	}
	req := p.requests[id]
	if done := p.skipUnchanged(reg, req); done {
		return
	}
	if err := p.local.SetScanPending(id, true); err != nil {
		p.fail(id, fmt.Errorf("journal pending scan: %w", err))
		return
	}
	// The session's published state, read once for the whole scan.
	published, err := p.local.LoadPublishedState(id)
	if err != nil {
		p.fail(id, fmt.Errorf("load published cache: %w", err))
		return
	}
	if err := announceSubagent(p.local, reg, published); err != nil {
		addError(p.result.Errors, id, err)
		p.pending++
	}
	scan := newSessionScan(p.ctx, p.local, p.remote, reg, req, published, p.now, p.opts)
	outcome, err := scan.run()
	for _, warning := range scan.warnings {
		addError(p.result.Errors, id, warning)
	}
	if err != nil && p.ctx.Err() != nil {
		// The pass ran out of time (or was cancelled) with this session in
		// flight. That is not the session failing: its pending publication
		// is intact and the next pass carries on with it.
		p.pending++
		p.opts.progress(id, false)
		return
	}
	if err != nil {
		p.fail(id, err)
		return
	}
	p.finishScan(id)
	switch outcome {
	case outcomePublished:
		p.result.Published = append(p.result.Published, id)
		if err := announceSubagent(p.local, reg, published); err != nil {
			addError(p.result.Errors, id, err)
			p.pending++
		}
	case outcomeRateLimited:
		p.result.Waiting = append(p.result.Waiting, id)
		if !scan.readyAt.IsZero() && (p.result.NextReadyAt.IsZero() || scan.readyAt.Before(p.result.NextReadyAt)) {
			p.result.NextReadyAt = scan.readyAt
		}
	case outcomeSkipped:
		p.result.Skipped = append(p.result.Skipped, id)
	}
	p.opts.progress(id, outcome == outcomePublished)
}

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
	unchanged, err := unchangedSinceLastScan(p.ctx, p.local, reg, p.opts)
	if err != nil {
		p.fail(id, fmt.Errorf("check transcript for changes: %w", err))
		return true
	}
	failed, failure := false, ""
	if unchanged {
		if failed, failure, err = rememberedFailure(p.local, reg); err != nil {
			p.fail(id, fmt.Errorf("check transcript for changes: %w", err))
			return true
		}
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
	// Settled, or a recorded gap (a size limit) at the same state. A request
	// on the gap is completed without a read, as a block completes it: the
	// chat is still over the limit, and the gap stands until the chat
	// changes.
	if req.Token != "" {
		if _, err := p.local.CompleteRequest(id, req.Token); err != nil {
			p.fail(id, fmt.Errorf("complete a request on an unchanged gap: %w", err))
			return true
		}
	}
	// Nothing to read, nothing to compare, nothing to journal.
	p.result.Skipped = append(p.result.Skipped, id)
	p.opts.progress(id, false)
	return true
}

// finishScan clears the scan journal once the session owes no further work,
// and counts it as pending otherwise. The session's own outcome stands
// whatever happens here: a publication has reached storage even if this
// bookkeeping fails, and the next pass redoes it.
func (p *pass) finishScan(id string) {
	_, requestPending, requestErr := p.local.LoadRequest(id)
	uploadPending, uploadErr := p.local.HasPending(id)
	switch {
	case requestErr != nil || uploadErr != nil:
		addError(p.result.Errors, id, fmt.Errorf("check outstanding work: %w", errors.Join(requestErr, uploadErr)))
		p.pending++
	case !requestPending && !uploadPending:
		if err := p.local.SetScanPending(id, false); err != nil {
			addError(p.result.Errors, id, fmt.Errorf("complete pending scan: %w", err))
			p.pending++
		}
	default:
		p.pending++
	}
}

// saveStatus records the pass's summary for `status`.
func (p *pass) saveStatus() error {
	// A status file that no longer decodes is replaced below; it only ever
	// carries the previous pass's summary.
	previous, err := p.local.LoadStatus()
	if err != nil && !state.IsUndecodable(err) {
		return err
	}
	lastPublishedAt := previous.LastPublishedAt
	if len(p.result.Published) > 0 {
		lastPublishedAt = p.now.UTC()
	}
	var lastError string
	if len(p.result.Errors) > 0 {
		lastError = fmt.Sprintf("%d session(s) failed to scan or publish", len(p.result.Errors))
	}
	status := state.Status{
		LastScanAt:             p.now.UTC(),
		PendingCount:           p.pending,
		LastPublishedAt:        lastPublishedAt,
		LastError:              lastError,
		QuarantinedFiles:       p.local.QuarantinedFiles(),
		UnrefreshableSummaries: p.local.CountRefreshSkips(p.opts.parserVersion()),
	}
	if err := p.local.SaveStatus(status); err != nil {
		return fmt.Errorf("save status: %w", err)
	}
	return nil
}

func (o Options) progress(archiveSessionID string, published bool) {
	if o.Progress != nil {
		o.Progress(Progress{ArchiveSessionID: archiveSessionID, Published: published})
	}
}

// addError records err against a session, keeping any error the pass already
// recorded for it.
func addError(errs map[string]error, archiveSessionID string, err error) {
	if previous := errs[archiveSessionID]; previous != nil {
		err = errors.Join(previous, err)
	}
	errs[archiveSessionID] = err
}

// orderOldestRequestsFirst puts the sessions with a pending request first,
// oldest start first, and leaves the rest in archive session ID order. An
// import queues its whole history at once; the oldest transcripts are the
// ones the apps delete next, so they are uploaded first.
func orderOldestRequestsFirst(registrations []archive.SessionRegistration, requests map[string]state.Request) {
	sort.SliceStable(registrations, func(i, j int) bool {
		a, b := registrations[i], registrations[j]
		aRequested, bRequested := requests[a.ArchiveSessionID].Token != "", requests[b.ArchiveSessionID].Token != ""
		if aRequested != bRequested {
			return aRequested
		}
		return aRequested && a.SessionStartedAt.Before(b.SessionStartedAt)
	})
}

// cursorTextSourceFormat labels a Cursor transcript captured as plain text.
const cursorTextSourceFormat = "cursor-text"

// harnessAdapterVersion returns the version of the adapter that reads
// harness, or known=false when no adapter does.
func harnessAdapterVersion(harness string) (string, bool) {
	adapter, err := archive.NewAdapter(harness)
	if err != nil {
		return "", false
	}
	return adapter.Version(), true
}

// unchangedSinceLastScan answers the spec's "transcript changed?" question
// without opening, reading, parsing, or journaling anything: one stat of the
// transcript plus two tiny local files. It is the difference between a pass
// costing time proportional to every session this machine has ever registered
// and one costing time proportional to the sessions that actually moved.
//
// It says yes only when all of the following hold, because each of them is a
// way an unchanged file can still owe work:
//
//   - The last completed scan left a signature (see ScanSignature). A blocked
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
//
// A Cursor database chat is identified by its cursorstore.Signature instead
// of a stat (see sourceReader).
//
// A Cursor chat whose last read failed in a way reading it again can't fix
// (see rememberFailedRead) is also "unchanged" while its signature and the
// versions match, although that failed scan is still journaled; Run reports
// its failure again without reading it.
func unchangedSinceLastScan(ctx context.Context, local *state.Store, reg archive.SessionRegistration, opts Options) (bool, error) {
	if reg.ParentSessionID != "" {
		return false, nil
	}
	reader, ok := newSourceReader(reg, opts)
	if !ok {
		return false, nil
	}
	signature, found, err := local.LoadScanSignature(reg.ArchiveSessionID)
	if err != nil || !found {
		return false, err
	}
	if signature.SourceFormat == cursorTextSourceFormat {
		return false, nil
	}
	adapterVersion, known := harnessAdapterVersion(reg.Harness.Name)
	if !known {
		return false, nil
	}
	if signature.ParserVersion != opts.parserVersion() || signature.FilterVersion != archive.FilterVersion || signature.AdapterVersion != adapterVersion {
		return false, nil
	}
	if signature.Failed && (signature.FailedMaxBytes != opts.maxTranscriptBytes() || signature.FailedRecordLimit != recordLimit) {
		return false, nil
	}
	observed, ok := reader.Signature(ctx)
	if !ok || !observed.matches(signature) {
		return false, nil
	}
	if !signature.Failed {
		if scanPending, err := local.ScanPending(reg.ArchiveSessionID); err != nil || scanPending {
			return false, err
		}
	}
	pending, err := local.HasPending(reg.ArchiveSessionID)
	if err != nil || pending {
		return false, err
	}
	return true, nil
}

// rememberedFailure reports whether unchangedSinceLastScan skipped a session
// on a remembered failure (see rememberFailedRead), and that failure's text:
// "" for a recorded gap.
func rememberedFailure(local *state.Store, reg archive.SessionRegistration) (failed bool, message string, err error) {
	if reg.SourceKind != archive.SourceKindCursorSQLite {
		return false, "", nil
	}
	signature, found, err := local.LoadScanSignature(reg.ArchiveSessionID)
	if err != nil || !found || !signature.Failed {
		return false, "", err
	}
	return true, signature.FailedError, nil
}

// recordScanSignature marks a session settled at the transcript bytes this
// scan consumed, so the next pass can skip it. It is written only at an exit
// that owes no further work.
func recordScanSignature(local *state.Store, reg archive.SessionRegistration, observed sourceState, bundle archive.SourceBundle, opts Options) error {
	return local.SaveScanSignature(reg.ArchiveSessionID, state.ScanSignature{
		TranscriptSize: observed.file.Size, TranscriptMtime: observed.file.Mtime,
		ParserVersion: opts.parserVersion(), FilterVersion: bundle.Capture.FilterVersion,
		AdapterVersion: bundle.Capture.AdapterVersion, SourceFormat: bundle.Capture.SourceFormat,
		SourceKind: observed.kind, CursorLastUpdatedAt: observed.cursor.LastUpdatedAt,
		CursorHeaderCount: observed.cursor.HeaderCount, CursorLastBubbleID: observed.cursor.LastBubbleID,
		CursorMessageRows: observed.cursor.MessageRows, CursorLastMessageHash: observed.cursor.LastMessageHash,
	})
}
