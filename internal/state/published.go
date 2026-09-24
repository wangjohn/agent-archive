package state

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

// CacheStatus distinguishes why a bundle sits in the local published cache,
// since only some of those reasons should be auto-retried once time passes.
type CacheStatus string

const (
	// CacheStatusPublished means this exact bundle was actually published.
	CacheStatusPublished CacheStatus = "published"
	// CacheStatusRateLimited means this bundle was built and differs from
	// what's published, but was withheld by the minimum upload interval; it
	// is eligible to auto-publish once that interval elapses.
	CacheStatusRateLimited CacheStatus = "rate_limited"
	// CacheStatusDeclined means this bundle was deliberately not published
	// by policy (see collector.Options.RequireSkillUse), not by cadence.
	// Unlike CacheStatusRateLimited, it must never auto-publish just because
	// time passed — only a genuine further content change reconsiders it.
	CacheStatusDeclined CacheStatus = "declined"
	// CacheStatusBlocked means the session's current transcript can no longer
	// be captured safely (see BlockedReason) and, unlike a transient failure,
	// the condition cannot clear by retrying: it is a recorded capture gap,
	// not an error. The last published snapshot stays retained and any
	// outstanding request is acknowledged, so later passes are no-ops until
	// the transcript changes again.
	CacheStatusBlocked CacheStatus = "blocked"
)

// BlockedReason says why a session sits in CacheStatusBlocked.
type BlockedReason string

const (
	// BlockedReasonTranscriptRewritten means the transcript was truncated,
	// compacted, or rewritten so it no longer extends the retained evidence.
	BlockedReasonTranscriptRewritten BlockedReason = "transcript_rewritten"
	// BlockedReasonTranscriptTooLarge means the transcript exceeds the
	// collection size limit (collector.Options.MaxTranscriptBytes).
	BlockedReasonTranscriptTooLarge BlockedReason = "transcript_too_large"
	// BlockedReasonRecordTooLarge means one record of the transcript is longer
	// than archive.MaxRecordBytes, so the transcript cannot be read whole.
	BlockedReasonRecordTooLarge BlockedReason = "record_size_limit"
	// BlockedReasonTranscriptMissing means the native transcript is no longer
	// on disk. Every supported application deletes its own transcripts on its
	// own schedule (Claude Code after cleanupPeriodDays, 30 by default) while
	// this archive retains sessions for far longer, so a session outliving its
	// transcript is the steady state, not a failure. Unlike the other reasons
	// this one can end: if the file comes back, the next scan clears the block.
	BlockedReasonTranscriptMissing BlockedReason = "transcript_missing"
)

// Recoverable reports whether a block can end without the session changing:
// only a missing file can reappear. A rewritten or oversize transcript stays
// rewritten or oversize until its content changes, which clears the block
// through the normal comparison instead.
func (r BlockedReason) Recoverable() bool { return r == BlockedReasonTranscriptMissing }

// publishedState is the small local cache of what was last built for a
// session: the exact source bundle (so a later scan can detect "no
// meaningful change" without redownloading or reparsing published history),
// when that happened, and why the bundle is in the state it's in. It is the
// JSON of published/<id>.json.
type publishedState struct {
	MetadataBytes []byte               `json:"metadata_bytes,omitempty"`
	Bundle        archive.SourceBundle `json:"bundle"`
	PublishedAt   time.Time            `json:"published_at"`
	Status        CacheStatus          `json:"status"`
	// BlockedReason is set only while Status is CacheStatusBlocked.
	BlockedReason BlockedReason `json:"blocked_reason,omitempty"`
	// PreBlockStatus is the status a recoverable block replaced, so clearing
	// that block restores what was true before it rather than guessing.
	PreBlockStatus CacheStatus `json:"pre_block_status,omitempty"`
	// DeferredHookEvidence is hook evidence (a final response, a link) whose
	// request was acknowledged while a recoverable block was in force. The
	// transcript could not be read, so nothing could be built to carry it;
	// rather than drop it, the block holds it and hands it back as request
	// evidence the moment the transcript is readable again, so it publishes
	// with the recovery. Set only while Status is CacheStatusBlocked with a
	// recoverable reason.
	DeferredHookEvidence []archive.SupplementalEvidence `json:"deferred_hook_evidence,omitempty"`
	// LastPublished survives a newer rate-limited or declined candidate so
	// compaction checks and retention always have the actual remote baseline.
	LastPublished *publishedSnapshot `json:"last_published,omitempty"`
}

type publishedSnapshot struct {
	Bundle      archive.SourceBundle `json:"bundle"`
	PublishedAt time.Time            `json:"published_at"`
	// Source is the source object this publication's metadata points at:
	// exactly the key, digest, and size that were uploaded. The next
	// publication names the object it supersedes from it. Rebuilding that
	// reference from Bundle is not reliable: a later build may serialize or
	// compress an old bundle differently, or refuse it outright after a
	// source schema bump. State written before this field existed is read
	// through its cached metadata instead (see lastPublishedSource).
	Source *archive.SourceReference `json:"source,omitempty"`
	// SameAsBundle means the last published bundle is the one in Bundle, so
	// this snapshot carries only its time. While a session sits in its normal
	// published state the two are always identical, and a source bundle is by
	// far the largest thing in this file: storing it once halves the file and
	// the cost of every decode of it. Bundle is materialized here again the
	// moment a different candidate (rate limited, declined, blocked) takes
	// over publishedState.Bundle. State written before this field existed
	// always carries its own copy, so it keeps working unchanged.
	SameAsBundle bool `json:"same_as_bundle,omitempty"`
}

// resolveLastPublished returns the bundle actually made discoverable remotely,
// expanding the shared-copy marker.
func (p publishedState) resolveLastPublished() (archive.SourceBundle, time.Time, bool) {
	if p.LastPublished == nil {
		// Backward compatibility with state written before the separate ledger.
		if p.Status == CacheStatusPublished {
			return p.Bundle, p.PublishedAt, true
		}
		return archive.SourceBundle{}, time.Time{}, false
	}
	if p.LastPublished.SameAsBundle {
		return p.Bundle, p.LastPublished.PublishedAt, true
	}
	return p.LastPublished.Bundle, p.LastPublished.PublishedAt, true
}

// detachedLastPublished gives the last published snapshot its own copy of the
// bundle, for use when publishedState.Bundle is about to become a different
// candidate. Called on every save that is not itself a publication.
func (p publishedState) detachedLastPublished() *publishedSnapshot {
	if p.LastPublished == nil || !p.LastPublished.SameAsBundle {
		return p.LastPublished
	}
	return &publishedSnapshot{Bundle: p.Bundle, PublishedAt: p.LastPublished.PublishedAt, Source: p.LastPublished.Source}
}

// lastPublishedSource returns the source reference of the last publication,
// as it was uploaded. found is false when nothing was published, or when
// state written before publishedSnapshot.Source existed has no readable
// cached metadata either: the reference is then unknown. It is never rebuilt.
func (p publishedState) lastPublishedSource() (archive.SourceReference, bool) {
	if _, _, published := p.resolveLastPublished(); !published {
		return archive.SourceReference{}, false
	}
	if p.LastPublished != nil && p.LastPublished.Source != nil {
		return *p.LastPublished.Source, true
	}
	// Older state: MetadataBytes is only ever replaced by a publication, so it
	// is the metadata document that went out with the last published source.
	if len(p.MetadataBytes) == 0 {
		return archive.SourceReference{}, false
	}
	// Only the reference is read, so metadata written under an older (or
	// newer) metadata schema still yields it.
	var metadata struct {
		SourceBundle archive.SourceReference `json:"source_bundle"`
	}
	if err := json.Unmarshal(p.MetadataBytes, &metadata); err != nil || !validSourceReference(metadata.SourceBundle) {
		return archive.SourceReference{}, false
	}
	return metadata.SourceBundle, true
}

// validSourceReference reports whether ref names an object and carries a
// SHA-256 digest in hex.
func validSourceReference(ref archive.SourceReference) bool {
	if ref.Key == "" || len(ref.SHA256) != 64 {
		return false
	}
	_, err := hex.DecodeString(ref.SHA256)
	return err == nil
}

// next returns the state that saving bundle with status makes of p: the
// transition every save goes through. publishedAt is the last actual publish
// time. metadata, when non-empty, replaces the cached metadata (only a
// publication supplies it). reason and deferred apply to CacheStatusBlocked,
// source to CacheStatusPublished (the uploaded source, when known).
//
// The last published snapshot survives any status but a publication, which
// replaces it; a recoverable block remembers the status it replaced and
// accumulates the hook evidence it held back while it lasts.
func (p publishedState) next(bundle archive.SourceBundle, publishedAt time.Time, status CacheStatus, reason BlockedReason, metadata []byte, deferred []archive.SupplementalEvidence, source *archive.SourceReference) publishedState {
	last := p.detachedLastPublished()
	if last == nil && p.Status == CacheStatusPublished {
		copy := publishedSnapshot{Bundle: p.Bundle, PublishedAt: p.PublishedAt}
		last = &copy
	}
	if status == CacheStatusPublished {
		// The candidate becoming the current bundle is exactly what was just
		// published, so the snapshot records only when, not a second copy.
		last = &publishedSnapshot{PublishedAt: publishedAt, SameAsBundle: true, Source: source}
	}
	preBlock := p.PreBlockStatus
	switch {
	case status != CacheStatusBlocked:
		preBlock = ""
	case p.Status != CacheStatusBlocked:
		preBlock = p.Status
	}
	var held []archive.SupplementalEvidence
	if status == CacheStatusBlocked && reason.Recoverable() {
		// The same block continuing keeps what it already holds; a block
		// that replaces a settled state starts with only what arrived now.
		if p.Status == CacheStatusBlocked && p.BlockedReason == reason {
			held = p.DeferredHookEvidence
		}
		held = archive.MergeSupplementalEvidence(held, deferred)
	}
	if len(metadata) == 0 {
		metadata = p.MetadataBytes
	}
	return publishedState{Bundle: bundle, PublishedAt: publishedAt, Status: status, BlockedReason: reason, PreBlockStatus: preBlock, DeferredHookEvidence: held, LastPublished: last, MetadataBytes: metadata}
}

// Published is one session's published state, read from published/<id>.json
// once and kept in step with every change made through it. The file holds a
// whole source bundle, often megabytes, so a collector scan loads one per
// session and passes it through its steps instead of decoding the file at
// each of them. It assumes it is that file's only writer while in use, which
// the collector lock guarantees for a pass.
type Published struct {
	store *Store
	id    string
	state publishedState
	found bool
}

func (s *Store) publishedPath(archiveSessionID string) string {
	return filepath.Join(s.home, "published", archiveSessionID+".json")
}

// LoadPublishedState reads a session's published state. A session with none
// yet yields an empty one (Found reports false) that saves create.
func (s *Store) LoadPublishedState(archiveSessionID string) (*Published, error) {
	if !safeFileComponent(archiveSessionID) {
		return nil, errors.New("archive session ID is not a safe file name component")
	}
	p := &Published{store: s, id: archiveSessionID}
	err := local.Read(s.publishedPath(archiveSessionID), &p.state)
	switch {
	case err == nil:
		p.found = true
	case errors.Is(err, os.ErrNotExist):
		p.state = publishedState{}
	default:
		return nil, fmt.Errorf("read published state %q: %w", archiveSessionID, err)
	}
	return p, nil
}

func (p *Published) write(next publishedState) error {
	if err := local.Write(p.store.publishedPath(p.id), next); err != nil {
		return err
	}
	p.state, p.found = next, true
	return nil
}

// Found reports whether anything has been saved for the session.
func (p *Published) Found() bool { return p.found }

// Cached returns the bundle a scan compares against and why it is there:
// the last publication, or a newer candidate (rate limited, declined,
// blocked). found is false when nothing has been saved.
func (p *Published) Cached() (bundle archive.SourceBundle, publishedAt time.Time, status CacheStatus, found bool) {
	return p.state.Bundle, p.state.PublishedAt, p.state.Status, p.found
}

// LastPublished returns the most recent bundle actually made discoverable
// by remote metadata. It deliberately ignores a newer local-only candidate.
func (p *Published) LastPublished() (bundle archive.SourceBundle, publishedAt time.Time, found bool) {
	return p.state.resolveLastPublished()
}

// LastPublishedSource returns the source reference of the last publication,
// exactly as uploaded: the object its live metadata points at. found is
// false when nothing was published, or when state from an old version
// records no reference (see publishedState.lastPublishedSource); callers
// must then not assume one, least of all by rebuilding it.
func (p *Published) LastPublishedSource() (archive.SourceReference, bool) {
	return p.state.lastPublishedSource()
}

// Metadata returns the metadata document published with the last
// publication, or nil when none is cached (publications from before it was).
func (p *Published) Metadata() []byte { return p.state.MetadataBytes }

// Blocked reports whether the session is in CacheStatusBlocked, and why.
func (p *Published) Blocked() (BlockedReason, bool) {
	if p.state.Status != CacheStatusBlocked {
		return "", false
	}
	return p.state.BlockedReason, true
}

// Save records the outcome of a build/publish decision, so the next scan can
// compare against it instead of rebuilding from scratch. See CacheStatus for
// what each status means for retry. metadata, when given, replaces the
// cached metadata document.
func (p *Published) Save(bundle archive.SourceBundle, publishedAt time.Time, status CacheStatus, metadata ...[]byte) error {
	var doc []byte
	if len(metadata) > 0 {
		doc = metadata[0]
	}
	return p.write(p.state.next(bundle, publishedAt, status, "", doc, nil, nil))
}

// SavePublication records a completed publication: bundle becomes both the
// comparison baseline and the last published snapshot, source is the object
// its metadata points at, and metadata is the document that was uploaded.
func (p *Published) SavePublication(bundle archive.SourceBundle, publishedAt time.Time, source archive.SourceReference, metadata []byte) error {
	return p.write(p.state.next(bundle, publishedAt, CacheStatusPublished, "", metadata, nil, &source))
}

// SaveBlocked records a terminal capture gap (see CacheStatusBlocked).
// bundle is what the next scan compares against and publishedAt is the last
// actual publish time, if any; the last published snapshot itself is
// preserved exactly as Save preserves it. For a recoverable reason, deferred
// is hook evidence the block acknowledged and must hand back when it clears;
// it accumulates across saves of the same block and is dropped, having been
// handed back, by any other status.
func (p *Published) SaveBlocked(bundle archive.SourceBundle, publishedAt time.Time, reason BlockedReason, deferred ...archive.SupplementalEvidence) error {
	if reason == "" {
		return errors.New("blocked reason is required")
	}
	return p.write(p.state.next(bundle, publishedAt, CacheStatusBlocked, reason, nil, deferred, nil))
}

// ClearRecoverableBlock ends a block whose condition has passed — today only a
// transcript that came back — by restoring the status the block replaced. It
// exists because a returning transcript whose content is byte-identical to the
// cached bundle produces no change for the normal comparison to act on, so
// without this the gap would be reported forever.
//
// Hook evidence the block held on to (see publishedState.DeferredHookEvidence)
// is handed back first, as an urgent request for the session, before the
// state is rewritten: the request is the durable carrier the collector
// already retries, so a crash at any point leaves the evidence pending
// rather than lost, and a replay adds nothing the request already holds.
// replayed reports that such a request was written, so the caller must
// reload the session's request rather than act on the one it loaded before.
//
// A block with no recorded previous status is not rewritten: there was no
// cached evidence before it, so the first real candidate replaces the whole
// state anyway, and anything it held is replayed (idempotently) until then.
func (p *Published) ClearRecoverableBlock(now time.Time) (restored CacheStatus, replayed, cleared bool, err error) {
	if !p.found {
		return "", false, false, nil
	}
	state := p.state
	if state.Status != CacheStatusBlocked || !state.BlockedReason.Recoverable() {
		return state.Status, false, false, nil
	}
	if len(state.DeferredHookEvidence) > 0 {
		if err := p.store.SaveRequest(p.id, "transcript-returned", now, state.DeferredHookEvidence...); err != nil {
			return "", false, false, fmt.Errorf("replay evidence held while blocked %q: %w", p.id, err)
		}
		replayed = true
	}
	if state.PreBlockStatus == "" || state.PreBlockStatus == CacheStatusBlocked {
		return state.Status, replayed, false, nil
	}
	restored = state.PreBlockStatus
	state.Status, state.BlockedReason, state.PreBlockStatus, state.DeferredHookEvidence = restored, "", "", nil
	if err := p.write(state); err != nil {
		return "", replayed, false, fmt.Errorf("clear block %q: %w", p.id, err)
	}
	return restored, replayed, true, nil
}

// CacheMetadata stores the metadata document of the last publication, for
// state written before it was cached. An unchanged document is not rewritten.
func (p *Published) CacheMetadata(metadata []byte) error {
	if !p.found {
		return fmt.Errorf("read published state %q: %w", p.id, os.ErrNotExist)
	}
	if bytes.Equal(p.state.MetadataBytes, metadata) {
		return nil
	}
	next := p.state
	next.MetadataBytes = metadata
	return p.write(next)
}

// SaveRepublishedMetadata records a metadata-only publication. Updating a
// summary must not discard a richer local-only candidate, so a rate-limited,
// declined, or blocked bundle stays the comparison baseline.
func (p *Published) SaveRepublishedMetadata(pending PendingPublication, at time.Time) error {
	if !p.found {
		return fmt.Errorf("read published state %q: %w", p.id, os.ErrNotExist)
	}
	next := p.state
	next.MetadataBytes = pending.MetadataBytes
	next.PublishedAt = at
	source := pending.SourceReference()
	if next.Status == CacheStatusPublished {
		// The current bundle is the republished one, so the snapshot shares it.
		next.Bundle = pending.Bundle
		next.LastPublished = &publishedSnapshot{PublishedAt: at, SameAsBundle: true, Source: &source}
	} else {
		next.LastPublished = &publishedSnapshot{Bundle: pending.Bundle, PublishedAt: at, Source: &source}
	}
	return p.write(next)
}

// The Store methods below read the published state, act on it, and (for the
// saves) write it back: one decode per call. Callers outside a collector
// scan use them; a scan uses one Published for the whole session instead.

// SavePublished is Published.Save for one session.
func (s *Store) SavePublished(archiveSessionID string, bundle archive.SourceBundle, publishedAt time.Time, status CacheStatus, metadata ...[]byte) error {
	p, err := s.LoadPublishedState(archiveSessionID)
	if err != nil {
		return err
	}
	return p.Save(bundle, publishedAt, status, metadata...)
}

// SavePublication is Published.SavePublication for one session.
func (s *Store) SavePublication(archiveSessionID string, bundle archive.SourceBundle, publishedAt time.Time, source archive.SourceReference, metadata []byte) error {
	p, err := s.LoadPublishedState(archiveSessionID)
	if err != nil {
		return err
	}
	return p.SavePublication(bundle, publishedAt, source, metadata)
}

// SaveBlocked is Published.SaveBlocked for one session.
func (s *Store) SaveBlocked(archiveSessionID string, bundle archive.SourceBundle, publishedAt time.Time, reason BlockedReason, deferred ...archive.SupplementalEvidence) error {
	if reason == "" {
		return errors.New("blocked reason is required")
	}
	p, err := s.LoadPublishedState(archiveSessionID)
	if err != nil {
		return err
	}
	return p.SaveBlocked(bundle, publishedAt, reason, deferred...)
}

// ClearRecoverableBlock is Published.ClearRecoverableBlock for one session.
func (s *Store) ClearRecoverableBlock(archiveSessionID string, now time.Time) (restored CacheStatus, replayed, cleared bool, err error) {
	p, err := s.LoadPublishedState(archiveSessionID)
	if err != nil {
		return "", false, false, err
	}
	return p.ClearRecoverableBlock(now)
}

// LoadBlocked reports whether a session is in CacheStatusBlocked and why.
func (s *Store) LoadBlocked(archiveSessionID string) (BlockedReason, bool, error) {
	p, err := s.LoadPublishedState(archiveSessionID)
	if err != nil {
		return "", false, err
	}
	reason, blocked := p.Blocked()
	return reason, blocked, nil
}

// LoadPublished returns the last cached bundle for a session, if any.
func (s *Store) LoadPublished(archiveSessionID string) (bundle archive.SourceBundle, publishedAt time.Time, status CacheStatus, found bool, err error) {
	p, err := s.LoadPublishedState(archiveSessionID)
	if err != nil {
		return archive.SourceBundle{}, time.Time{}, "", false, err
	}
	bundle, publishedAt, status, found = p.Cached()
	return bundle, publishedAt, status, found, nil
}

// LoadLastPublished returns the most recent bundle actually made discoverable
// by remote metadata. It deliberately ignores a newer local-only candidate.
func (s *Store) LoadLastPublished(archiveSessionID string) (bundle archive.SourceBundle, publishedAt time.Time, found bool, err error) {
	p, err := s.LoadPublishedState(archiveSessionID)
	if err != nil {
		return archive.SourceBundle{}, time.Time{}, false, err
	}
	bundle, publishedAt, found = p.LastPublished()
	return bundle, publishedAt, found, nil
}

// LoadLastPublishedSource is Published.LastPublishedSource for one session.
func (s *Store) LoadLastPublishedSource(archiveSessionID string) (archive.SourceReference, bool, error) {
	p, err := s.LoadPublishedState(archiveSessionID)
	if err != nil {
		return archive.SourceReference{}, false, err
	}
	source, found := p.LastPublishedSource()
	return source, found, nil
}

// CacheMetadata is Published.CacheMetadata for one session.
func (s *Store) CacheMetadata(archiveSessionID string, metadata []byte) error {
	p, err := s.LoadPublishedState(archiveSessionID)
	if err != nil {
		return err
	}
	return p.CacheMetadata(metadata)
}

// PublishedMetadata is Published.Metadata for one session. It reports
// os.ErrNotExist when nothing has been saved for the session.
func (s *Store) PublishedMetadata(archiveSessionID string) ([]byte, error) {
	p, err := s.LoadPublishedState(archiveSessionID)
	if err != nil {
		return nil, err
	}
	if !p.found {
		return nil, fmt.Errorf("read published state %q: %w", archiveSessionID, os.ErrNotExist)
	}
	return p.Metadata(), nil
}
