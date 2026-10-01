package collector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/sourceio"
	"github.com/wangjohn/agent-archive/internal/state"
)

// sourceState carries a provider observation and legacy equality facts.
type sourceState struct {
	kind        archive.SourceKind
	file        transcriptFileInfo
	cursor      cursorstore.Signature
	observation agentapi.SourceObservation
}

func (s sourceState) empty() bool {
	if s.observation.Present {
		return s.observation.Empty
	}
	if s.kind == archive.SourceKindCursorSQLite {
		return s.cursor.HeaderCount == 0
	}
	return s.file.Size == 0
}
func (s sourceState) matches(old state.ScanSignature) bool {
	if old.SourceKind != s.kind {
		return false
	}
	if s.observation.Present {
		if old.SourceSignature != nil {
			return *old.SourceSignature == s.observation.Signature
		}
		if s.kind == archive.SourceKindCursorSQLite {
			return sourceio.CursorSignature(old.CursorSignature()) == s.observation.Signature
		}
		return sourceio.FileSignature(old.TranscriptSize, old.TranscriptMtime) == s.observation.Signature
	}
	if s.kind == archive.SourceKindCursorSQLite {
		return old.CursorSignature() == s.cursor
	}
	return s.file == transcriptFileInfo{Size: old.TranscriptSize, Mtime: old.TranscriptMtime}
}
func observe(kind archive.SourceKind, o agentapi.SourceObservation) sourceState {
	s := sourceState{kind: kind, observation: o}
	if kind == archive.SourceKindFile {
		s.file = transcriptFileInfo{Size: o.Size, Mtime: o.Activity.UnixNano()}
	}
	return s
}
func sourceRef(reg archive.SessionRegistration) agentapi.SourceRef {
	return agentapi.SourceRef{Kind: reg.SourceKind, Path: reg.TranscriptPath, Key: reg.SourceKey}
}

// sourceReader remains a small compatibility seam while policy consumes provider values.
type sourceReader interface {
	Signature(context.Context) (sourceState, error)
	Filter(context.Context, archive.Adapter, int64) (archive.FilteredTranscript, sourceState, error)
}

func newSourceReader(reg archive.SessionRegistration, opts Options) (sourceReader, bool) {
	if reg.ReadsTranscriptFile() && reg.TranscriptPath == "" {
		return nil, false
	}
	return providerReader{reg: reg, opts: opts}, true
}

type providerReader struct {
	reg  archive.SessionRegistration
	opts Options
}

func (r providerReader) binding() (agentapi.SourceProvider, agentapi.TranscriptFilter, error) {
	if r.opts.Sources == nil {
		return nil, nil, errors.New("source integrations are required")
	}
	p, f, ok := r.opts.Sources.LookupSources(r.reg.Harness.Name)
	if !ok {
		return nil, nil, fmt.Errorf("source integration unavailable for %s", r.reg.Harness.Name)
	}
	if _, err := p.Describe(sourceRef(r.reg)); err != nil {
		return nil, nil, err
	}
	return p, f, nil
}
func (r providerReader) pass(ctx context.Context) (agentapi.SourcePass, func() error, error) {
	p, _, err := r.binding()
	if err != nil {
		return nil, nil, err
	}
	if r.opts.sourcePasses != nil {
		pass, err := r.opts.sourcePasses.get(ctx, r.reg.Harness.Name, p)
		return pass, func() error { return nil }, err
	}
	pass, err := p.OpenPass(ctx, agentapi.SourceEnvironment{Database: r.opts.cursorDatabase()})
	if err != nil {
		return nil, nil, err
	}
	return pass, pass.Close, nil
}
func (r providerReader) Signature(ctx context.Context) (out sourceState, err error) {
	p, closePass, err := r.pass(ctx)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, closePass()) }()
	o, err := p.Signature(ctx, sourceRef(r.reg))
	if err == nil {
		err = sourceio.ValidateSignature(o.Signature)
	}
	return observe(r.reg.SourceKind, o), err
}
func (r providerReader) Filter(ctx context.Context, _ archive.Adapter, maxBytes int64) (out archive.FilteredTranscript, observed sourceState, err error) {
	_, f, err := r.binding()
	if err != nil {
		return out, observed, err
	}
	p, closePass, err := r.pass(ctx)
	if err != nil {
		return out, observed, err
	}
	defer func() { err = errors.Join(err, closePass()) }()
	limits := agentapi.ReadLimits{RawBytes: maxRawBytes(maxBytes), RecordBytes: recordLimit, SubagentMetadata: r.reg.ParentSessionID != ""}
	snap, err := p.Read(ctx, sourceRef(r.reg), limits)
	if err != nil {
		if o, ok := agentapi.ErrorObservation(err); ok {
			observed = observe(r.reg.SourceKind, o)
		}
		return out, observed, translateSourceError(err)
	}
	defer func() { err = errors.Join(err, snap.Close()) }()
	observed = observe(r.reg.SourceKind, snap.Observation())
	if err = sourceio.ValidateSignature(observed.observation.Signature); err != nil {
		return out, sourceState{}, err
	}
	if r.reg.ReadsTranscriptFile() {
		transcriptFilters.Add(1)
	}
	out, err = f.Filter(ctx, snap.Input(), agentapi.FilterContext{StartedAt: r.reg.SessionStartedAt, Limits: limits})
	if err != nil {
		return out, observed, translateSourceError(err)
	}
	return out, observed, checkFilteredSize(out, maxBytes)
}
func translateSourceError(err error) error {
	switch {
	case errors.Is(err, agentapi.ErrRawLimit):
		return errors.Join(errTranscriptTooLarge, err)
	case errors.Is(err, archive.ErrRecordTooLarge), errors.Is(err, cursorstore.ErrRecordLimit):
		return errors.Join(errRecordTooLarge, err)
	}
	return err
}

// transcriptFilters counts full changed-file filtering, never previews.
var transcriptFilters atomic.Int64

// CursorChatSize counts existing native value bytes, excluding framing.
func CursorChatSize(c cursorstore.Composer) int64 {
	n := int64(len(c.Composer))
	for _, b := range c.Bubbles {
		n += int64(len(b.Value))
	}
	return n
}
func checkCursorChatSize(c cursorstore.Composer, maxBytes int64) error {
	if int64(len(c.Composer)) > recordLimit {
		return errRecordTooLarge
	}
	for _, b := range c.Bubbles {
		if int64(len(b.Value)) > recordLimit {
			return errRecordTooLarge
		}
	}
	if CursorChatSize(c) > maxRawBytes(maxBytes) {
		return errTranscriptTooLarge
	}
	return nil
}

// FilterCursorChat filters already materialized import data through an injected native filter.
func FilterCursorChat(c cursorstore.Composer, sources agentapi.SourcesLookup) (archive.FilteredTranscript, error) {
	if sources == nil {
		return archive.FilteredTranscript{}, errors.New("source integrations are required")
	}
	_, f, ok := sources.LookupSources(archive.HarnessCursor)
	if !ok {
		return archive.FilteredTranscript{}, errors.New("cursor filter unavailable")
	}
	if err := checkCursorChatSize(c, DefaultMaxTranscriptBytes); err != nil {
		return archive.FilteredTranscript{}, errors.Join(archive.ErrRecordTooLarge, err)
	}
	out, err := f.Filter(context.Background(), agentapi.NativeInput{Records: &composerRecords{c: c}}, agentapi.FilterContext{})
	if err != nil {
		return out, err
	}
	return out, checkFilteredSize(out, DefaultMaxTranscriptBytes)
}

type composerRecords struct {
	c cursorstore.Composer
	i int
}

func (r *composerRecords) Next(ctx context.Context) (agentapi.NativeRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return agentapi.NativeRecord{}, false, err
	}
	if r.i == 0 {
		r.i++
		return agentapi.NativeRecord{Kind: "composer", Raw: r.c.Composer}, true, nil
	}
	i := r.i - 1
	if i >= len(r.c.Bubbles) {
		return agentapi.NativeRecord{}, false, nil
	}
	b := r.c.Bubbles[i]
	r.i++
	return agentapi.NativeRecord{Kind: "bubble", Key: b.ID, Raw: b.Value, Missing: b.Value == nil}, true, nil
}

// cursorDatabase is Cursor's state.vscdb for this user.
func (o Options) cursorDatabase() string {
	if o.CursorDatabase != "" {
		return o.CursorDatabase
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return cursorstore.StateDatabase(home)
}

// openCursorPass gives the pass one Reader for Cursor's database when any
// session is read from it (Run has already swept snapshots a killed pass
// left behind).
// The returned function removes the pass's snapshot, and says if it could
// not: a copy of every Cursor chat left in the temporary directory is worth
// a failed pass (the next sweep removes it once it is stale).
type sourcePassSet struct {
	env    agentapi.SourceEnvironment
	passes map[string]agentapi.SourcePass
}

func (s *sourcePassSet) get(ctx context.Context, name string, p agentapi.SourceProvider) (agentapi.SourcePass, error) {
	if pass := s.passes[name]; pass != nil {
		return pass, nil
	}
	pass, err := p.OpenPass(ctx, s.env)
	if err == nil {
		s.passes[name] = pass
	}
	return pass, err
}
func openCursorPass(_ []archive.SessionRegistration, opts *Options) func() error {
	opts.sourcePasses = &sourcePassSet{env: agentapi.SourceEnvironment{Database: opts.cursorDatabase()}, passes: map[string]agentapi.SourcePass{}}
	return func() error {
		var err error
		for _, p := range opts.sourcePasses.passes {
			if counter, ok := p.(interface{ Snapshots() int }); ok && opts.afterCursorPass != nil {
				opts.afterCursorPass(counter.Snapshots())
			}
			err = errors.Join(err, p.Close())
		}
		return err
	}
}

// rememberFailedRead records, for a cursor-sqlite session whose chat was
// read but could not be captured for a reason that reading it again cannot
// change (a filter error, or a size limit), the state it failed at, with
// the error's text ("" for a recorded gap). unchangedSinceLastScan then
// skips the chat until its signature, or a derivation version, changes, so
// the failure costs one in-place signature read per pass rather than a copy
// of the database. A failure to read at all (a lock, a changed file) is
// left to be retried. The size limits in force are recorded too, so raising
// one reads the chat again, and so is the gap (blocked) a size limit
// recorded, for status.
func rememberFailedRead(local *state.Store, reg archive.SessionRegistration, adapter archive.Adapter, observed sourceState, opts Options, failure error, blocked state.BlockedReason) error {
	if !observed.observation.Present {
		return nil
	}
	message := ""
	if failure != nil && (!agentapi.Deterministic(failure) || (agentapi.Failure(failure) != agentapi.Unsafe && agentapi.Failure(failure) != agentapi.FormatMismatch && agentapi.Failure(failure) != agentapi.Limit)) {
		return nil
	}
	if failure != nil {
		message = failure.Error()
	}
	return local.SaveScanSignature(reg.ArchiveSessionID, state.ScanSignature{
		SkillEvidence: string(opts.skillEvidence()),
		ParserVersion: opts.parserVersion(), FilterVersion: archive.FilterVersion, AdapterVersion: adapter.Version(),
		SourceSignature: signaturePointer(observed), SourceKind: observed.kind, CursorLastUpdatedAt: observed.cursor.LastUpdatedAt,
		CursorHeaderCount: observed.cursor.HeaderCount, CursorLastBubbleID: observed.cursor.LastBubbleID,
		CursorMessageRows: observed.cursor.MessageRows, CursorLastMessageHash: observed.cursor.LastMessageHash,
		Failed: true, FailedError: message, FailedMaxBytes: opts.maxTranscriptBytes(), FailedRecordLimit: recordLimit,
		Blocked: blocked,
	})
}

// CaptureGapCursorChatRewritten marks a Cursor database chat whose messages
// changed after an earlier snapshot of it was taken, so a later snapshot
// replaced one that did not lead to it (see sessionScan.guard). A chat has at
// most one: its detail counts the rewrites, and its observation time is the
// last one's.
const CaptureGapCursorChatRewritten = "cursor_chat_rewritten"

// cursorRewriteProvenance is the rewrite gap's provenance: the collector's
// own, never a hook's, so it is not taken for a resume.
const cursorRewriteProvenance = "collector:cursor-rewrite"

// cursorRewriteDetail is the rewrite gap's detail for n rewrites. The
// earlier snapshot may have been published, or only held back (rate
// limited or declined), so the wording claims neither.
const cursorRewriteDetail = "Cursor changed messages of this chat after an earlier snapshot of it %d time(s); the snapshot is the chat as it was last read"

// withCursorRewriteGap returns evidence with the chat's one rewrite gap
// counting one more rewrite, observed at: an earlier rewrite gap is replaced,
// not added to, so a chat Cursor rewrites often (late token counts are
// routine) does not grow a gap per rewrite.
func withCursorRewriteGap(evidence []archive.SupplementalEvidence, at time.Time) []archive.SupplementalEvidence {
	rewrites := 0
	out := make([]archive.SupplementalEvidence, 0, len(evidence)+1)
	for _, e := range evidence {
		if e.Kind == archive.EvidenceKindCaptureGap && e.Provenance == cursorRewriteProvenance {
			n := 0
			detail, _ := e.Payload["detail"].(string)
			if _, err := fmt.Sscanf(detail, cursorRewriteDetail, &n); err != nil || n < 1 {
				n = 1
			}
			rewrites += n
			continue
		}
		out = append(out, e)
	}
	return append(out, archive.SupplementalEvidence{
		Kind: archive.EvidenceKindCaptureGap, ObservedAt: at.UTC(), Provenance: cursorRewriteProvenance,
		Payload: map[string]any{
			"code":   CaptureGapCursorChatRewritten,
			"detail": fmt.Sprintf(cursorRewriteDetail, rewrites+1),
		},
	})
}

// unchangedSinceFailureError reports a remembered failure again on a pass that
// skipped the chat because it has not changed since.
type unchangedSinceFailureError struct {
	message string
	cursor  bool
}

func (e unchangedSinceFailureError) Error() string {
	if e.cursor {
		return e.message + " (the Cursor chat has not changed since)"
	}
	return e.message + " (the source has not changed since)"
}

func signaturePointer(s sourceState) *agentapi.SourceSignature {
	if !s.observation.Present {
		return nil
	}
	v := s.observation.Signature
	return &v
}

func sourceSemantics(sources agentapi.SourcesLookup, reg archive.SessionRegistration) (agentapi.SourceSemantics, error) {
	if sources == nil {
		return agentapi.SourceSemantics{}, errors.New("source integrations are required")
	}
	p, _, ok := sources.LookupSources(reg.Harness.Name)
	if !ok {
		return agentapi.SourceSemantics{}, errors.New("source integration unavailable")
	}
	return p.Describe(sourceRef(reg))
}
