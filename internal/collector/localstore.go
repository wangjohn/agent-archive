// Package collector runs the local scan/build/publish loop that turns a
// hook-registered session into a published metadata sidecar and source
// bundle. It owns publication cadence and change detection; it builds on
// internal/local for the private home directory, atomic file I/O, and the
// machine-level lock, and does not own transcript reading (archive
// adapters), privacy filtering (archive adapters), or storage upload
// mechanics (storage.PutSourceThenMetadata). A caller runs Run under
// local.Lock(home) so only one collector process acts on a given home at a
// time; Run itself does not take that lock.
package collector

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

// LocalStore persists small operational files under a private home
// directory (see local.Home): registrations, upload requests, and a
// per-session cache of the last published source bundle, used to detect
// unchanged input without redownloading or reparsing published history. It
// never stores credentials or a second copy of conversation content beyond
// what the published source bundle itself already contains.
type LocalStore struct {
	home string
}

// NewLocalStore creates (if needed) the local store's directory layout under
// home — ordinarily the result of local.Home() — and returns a handle to it.
// home is caller-owned; this package never deletes it.
func OpenLocalStoreReadOnly(home string) *LocalStore { return &LocalStore{home: home} }

func NewLocalStore(home string) (*LocalStore, error) {
	if strings.TrimSpace(home) == "" {
		return nil, errors.New("local store home is required")
	}
	for _, dir := range []string{"registrations", "requests", "request-locks", "published", "pending", "sessions", "pending-scans", "scan-signatures", "subagent-candidates"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
			return nil, fmt.Errorf("create local store directory %q: %w", dir, err)
		}
	}
	return &LocalStore{home: home}, nil
}

func safeFileComponent(value string) bool {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\") {
		return false
	}
	return true
}

// SaveRegistration durably records a hook-observed session start. Re-saving
// the same archive session ID overwrites its prior registration.
func (s *LocalStore) SaveRegistration(reg archive.SessionRegistration) error {
	if err := reg.Validate(); err != nil {
		return err
	}
	if !safeFileComponent(reg.ArchiveSessionID) {
		return errors.New("archive session ID is not a safe file name component")
	}
	return local.Write(s.registrationPath(reg.ArchiveSessionID), reg)
}

// UpdateRegistration changes an existing registration under the per-session
// request lock, the lock retention holds while it forgets a session. It loads
// the registration under that lock and reports found=false, without calling
// update, when the registration is gone: a hook that looked the session up
// before the lock must then treat it as never seen. Otherwise update edits the
// loaded registration and it is saved; an error from update saves nothing and
// is returned as is.
//
// A plain load then SaveRegistration from a hook could write the registration
// back after retention forgot it, leaving it without its native-session index
// entry, so the session's next start would be given a second archive ID.
func (s *LocalStore) UpdateRegistration(archiveSessionID string, update func(*archive.SessionRegistration) error) (found bool, err error) {
	if !safeFileComponent(archiveSessionID) {
		return false, errors.New("archive session ID is not a safe file name component")
	}
	unlock, err := s.lockRequest(archiveSessionID)
	if err != nil {
		return false, err
	}
	defer unlock()
	reg, found, err := s.LoadRegistration(archiveSessionID)
	if err != nil || !found {
		return false, err
	}
	if err := update(&reg); err != nil {
		return true, err
	}
	return true, s.SaveRegistration(reg)
}

// RegisterNewSession assigns (or reuses) the archive session ID indexed for a
// native session and saves the registration build returns for it, under that
// ID's request lock. Under the lock it rechecks that the index still maps the
// native session to that ID: retention may have been forgetting a previous
// registration of the same native session, whose index entry the lookup saw
// just before retention removed it. Reusing that ID would leave the new
// registration with no index entry. If the entry changed or disappeared, a
// fresh ID is assigned and the check repeats.
func (s *LocalStore) RegisterNewSession(nativeSessionID string, build func(archiveSessionID string) archive.SessionRegistration) (archive.SessionRegistration, error) {
	for attempt := 0; attempt < 3; attempt++ {
		id, _, err := s.EnsureArchiveSessionID(nativeSessionID)
		if err != nil {
			return archive.SessionRegistration{}, err
		}
		reg, saved, err := s.registerUnderLock(nativeSessionID, id, build)
		if err != nil || saved {
			return reg, err
		}
	}
	return archive.SessionRegistration{}, errors.New("session index kept changing while registering; this start was not recorded")
}

func (s *LocalStore) registerUnderLock(nativeSessionID, id string, build func(string) archive.SessionRegistration) (archive.SessionRegistration, bool, error) {
	unlock, err := s.lockRequest(id)
	if err != nil {
		return archive.SessionRegistration{}, false, err
	}
	defer unlock()
	current, found, err := s.ArchiveSessionID(nativeSessionID)
	if err != nil || !found || current != id {
		return archive.SessionRegistration{}, false, err
	}
	reg := build(id)
	if reg.ArchiveSessionID != id || reg.NativeSessionID != nativeSessionID {
		return archive.SessionRegistration{}, false, errors.New("registration does not match the session index")
	}
	if err := s.SaveRegistration(reg); err != nil {
		return archive.SessionRegistration{}, false, err
	}
	return reg, true, nil
}

func (s *LocalStore) registrationPath(archiveSessionID string) string {
	return filepath.Join(s.home, "registrations", archiveSessionID+".json")
}

// LoadRegistrations returns every registered session, sorted by archive
// session ID for deterministic scan order.
func (s *LocalStore) LoadRegistrations() ([]archive.SessionRegistration, error) {
	dir := filepath.Join(s.home, "registrations")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list registrations: %w", err)
	}
	out := make([]archive.SessionRegistration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var reg archive.SessionRegistration
		if err := local.Read(filepath.Join(dir, entry.Name()), &reg); err != nil {
			return nil, fmt.Errorf("read registration %q: %w", entry.Name(), err)
		}
		out = append(out, reg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ArchiveSessionID < out[j].ArchiveSessionID })
	return out, nil
}

// LoadRegistration returns one session's registration, if it has been saved.
func (s *LocalStore) LoadRegistration(archiveSessionID string) (archive.SessionRegistration, bool, error) {
	var reg archive.SessionRegistration
	err := local.Read(s.registrationPath(archiveSessionID), &reg)
	if errors.Is(err, os.ErrNotExist) {
		return archive.SessionRegistration{}, false, nil
	}
	if err != nil {
		return archive.SessionRegistration{}, false, fmt.Errorf("read registration %q: %w", archiveSessionID, err)
	}
	return reg, true, nil
}

// Request is a small durable marker left by a stop, failure, or end hook. It
// exists to carry hook-only supplemental evidence (a final response, model
// selection) through to the next scan; periodic scanning already covers
// every registered session regardless of whether a request is pending, so a
// request with no new evidence is a no-op beyond confirming the session was
// checked.
type Request struct {
	ArchiveSessionID string                         `json:"archive_session_id"`
	Token            string                         `json:"token"`
	Reasons          []string                       `json:"reasons"`
	RequestedAt      time.Time                      `json:"requested_at"`
	HookEvidence     []archive.SupplementalEvidence `json:"hook_evidence,omitempty"`
	// Deferred marks a request that only carries lifecycle activity evidence
	// (a session start, a prompt submitted) and has not asked for a debounce
	// flush: the collector folds it in on its normal MinUploadInterval
	// schedule instead of uploading on every prompt. A stop, end, or response
	// request for the same session clears it. Requests written before the
	// field existed are urgent, as they always were.
	Deferred bool `json:"deferred,omitempty"`
}

// urgent reports whether the request asks the collector to flush the upload
// debounce now rather than wait for the next scheduled publication.
func (r Request) urgent() bool {
	return r.Token != "" && !r.Deferred
}

// SaveRequest merges a new hook event into any already-pending request for
// the same session: reasons accumulate, evidence appends, and RequestedAt
// advances to the latest event. This is the coalescing the spec asks for
// when a stop and a session-end hook both fire for the same session. The
// resulting request is urgent: the collector publishes it as soon as it is
// scanned, bypassing MinUploadInterval.
func (s *LocalStore) SaveRequest(archiveSessionID, reason string, requestedAt time.Time, evidence ...archive.SupplementalEvidence) error {
	return s.saveRequest(archiveSessionID, reason, requestedAt, false, evidence...)
}

// SaveEvidence appends lifecycle evidence to the session's request without
// asking for a flush. A request it creates is deferred; a request that is
// already pending keeps its urgency and RequestedAt, so a stop that is
// waiting to publish is neither delayed nor re-triggered by a later prompt.
func (s *LocalStore) SaveEvidence(archiveSessionID, reason string, observedAt time.Time, evidence ...archive.SupplementalEvidence) error {
	return s.saveRequest(archiveSessionID, reason, observedAt, true, evidence...)
}

// ErrSessionNotRegistered is returned when a request is written for a session
// whose registration no longer exists, typically because retention forgot it
// between the caller's own lookup and the write.
var ErrSessionNotRegistered = errors.New("session is no longer registered")

// lockRequest takes the per-session request lock. Hooks writing a request,
// the collector acknowledging one, and retention forgetting the session all
// hold it, so none of them can interleave with another.
func (s *LocalStore) lockRequest(archiveSessionID string) (func(), error) {
	unlock, err := local.NamedLockWait(s.home, requestLockName(archiveSessionID), time.Second)
	if err != nil {
		return nil, fmt.Errorf("lock request %q: %w", archiveSessionID, err)
	}
	return unlock, nil
}

func requestLockName(archiveSessionID string) string {
	return filepath.Join("request-locks", archiveSessionID+".lock")
}

// saveRequest always assigns a fresh token, even for deferred evidence: the
// token is what CompleteRequest checks, so a hook that lands while a scan is
// publishing the previous token keeps its evidence pending for the next
// pass instead of being acknowledged away with it. Evidence identical to an
// item the pending request already carries is dropped, and a call that adds
// nothing at all leaves the request untouched.
func (s *LocalStore) saveRequest(archiveSessionID, reason string, requestedAt time.Time, deferred bool, evidence ...archive.SupplementalEvidence) error {
	if !safeFileComponent(archiveSessionID) {
		return errors.New("archive session ID is not a safe file name component")
	}
	if requestedAt.IsZero() {
		return errors.New("requested_at is required")
	}
	unlock, err := s.lockRequest(archiveSessionID)
	if err != nil {
		return err
	}
	defer unlock()
	// Retention forgets a session under this same lock. A caller that looked
	// the registration up before taking the lock may be writing for a session
	// that is gone now; its request would be an orphan nothing ever reads.
	if _, err := os.Stat(s.registrationPath(archiveSessionID)); errors.Is(err, os.ErrNotExist) {
		return ErrSessionNotRegistered
	} else if err != nil {
		return fmt.Errorf("check registration %q: %w", archiveSessionID, err)
	}
	existing, found, err := s.loadRequest(archiveSessionID)
	if err != nil {
		return err
	}
	merged := Request{ArchiveSessionID: archiveSessionID, RequestedAt: requestedAt, Deferred: deferred}
	urgencyChanged := false
	if found {
		merged = existing
		if !deferred {
			urgencyChanged = merged.Deferred
			merged.Deferred = false
			if requestedAt.After(merged.RequestedAt) {
				merged.RequestedAt = requestedAt
			}
		}
	}
	merged.Token, err = local.ID()
	if err != nil {
		return fmt.Errorf("generate request token: %w", err)
	}
	reasonAlready := true
	if reason != "" {
		have := false
		for _, r := range merged.Reasons {
			if r == reason {
				have = true
				break
			}
		}
		if !have {
			reasonAlready = false
			merged.Reasons = append(merged.Reasons, reason)
		}
	}
	added := 0
	for _, item := range evidence {
		if requestHasEvidence(merged.HookEvidence, item) {
			continue
		}
		merged.HookEvidence = append(merged.HookEvidence, item)
		added++
	}
	// A repeated notification that changes nothing about the pending request
	// is not written back at all. A retry loop — the collector re-announcing a
	// published child to a parent whose own request never clears — would
	// otherwise append an identical evidence item on every pass and grow the
	// request file without bound.
	if found && existing.Token != "" && added == 0 && reasonAlready && !urgencyChanged && !requestedAt.After(existing.RequestedAt) {
		return nil
	}
	return local.Write(s.requestPath(archiveSessionID), merged)
}

func requestHasEvidence(have []archive.SupplementalEvidence, candidate archive.SupplementalEvidence) bool {
	for _, item := range have {
		if archive.SupplementalEvidenceEqual(item, candidate) {
			return true
		}
	}
	return false
}

func (s *LocalStore) requestPath(archiveSessionID string) string {
	return filepath.Join(s.home, "requests", archiveSessionID+".json")
}

func (s *LocalStore) loadRequest(archiveSessionID string) (Request, bool, error) {
	var req Request
	err := local.Read(s.requestPath(archiveSessionID), &req)
	if errors.Is(err, os.ErrNotExist) {
		return Request{}, false, nil
	}
	if err != nil {
		return Request{}, false, fmt.Errorf("read request %q: %w", archiveSessionID, err)
	}
	return req, true, nil
}

// ensureRequestToken upgrades a request written by an older collector. The
// token is assigned under the same lock used by hooks and acknowledgements so
// migration cannot overwrite a concurrent hook update. found is false when
// the request disappeared between listing and upgrade.
func (s *LocalStore) ensureRequestToken(archiveSessionID string) (Request, bool, error) {
	unlock, err := local.NamedLockWait(s.home, requestLockName(archiveSessionID), time.Second)
	if err != nil {
		return Request{}, false, fmt.Errorf("lock request %q: %w", archiveSessionID, err)
	}
	defer unlock()
	request, found, err := s.loadRequest(archiveSessionID)
	if err != nil || !found || request.Token != "" {
		return request, found, err
	}
	request.Token, err = local.ID()
	if err != nil {
		return Request{}, false, fmt.Errorf("generate request token: %w", err)
	}
	if err := local.Write(s.requestPath(archiveSessionID), request); err != nil {
		return Request{}, false, fmt.Errorf("upgrade request %q: %w", archiveSessionID, err)
	}
	return request, true, nil
}

// LoadRequests returns every pending request, sorted by archive session ID.
func (s *LocalStore) LoadRequests() ([]Request, error) {
	dir := filepath.Join(s.home, "requests")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list requests: %w", err)
	}
	out := make([]Request, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		req, found, err := s.loadRequest(id)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, req)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ArchiveSessionID < out[j].ArchiveSessionID })
	return out, nil
}

// CompleteRequest removes a request only if it still has the random token
// covered by a durable publish or policy decision. A hook that arrives while
// a scan is in progress assigns a new token under the same lock and therefore
// remains pending for the next pass. Tokens do not repeat when a request file
// was removed between events, unlike a per-file revision counter.
func (s *LocalStore) CompleteRequest(archiveSessionID, coveredToken string) (bool, error) {
	if !safeFileComponent(archiveSessionID) {
		return false, errors.New("archive session ID is not a safe file name component")
	}
	unlock, err := local.NamedLockWait(s.home, requestLockName(archiveSessionID), time.Second)
	if err != nil {
		return false, fmt.Errorf("lock request %q: %w", archiveSessionID, err)
	}
	defer unlock()
	current, found, err := s.loadRequest(archiveSessionID)
	if err != nil || !found {
		return false, err
	}
	if coveredToken == "" || current.Token != coveredToken {
		return false, nil
	}
	err = os.Remove(s.requestPath(archiveSessionID))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("remove request %q: %w", archiveSessionID, err)
	}
	return true, nil
}

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
	// by policy (see Options.RequireSkillUse), not by cadence. Unlike
	// CacheStatusRateLimited, it must never auto-publish just because time
	// passed — only a genuine further content change reconsiders it.
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
	// collection size limit (Options.MaxTranscriptBytes).
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

// recoverable reports whether a block can end without the session changing:
// only a missing file can reappear. A rewritten or oversize transcript stays
// rewritten or oversize until its content changes, which clears the block
// through the normal comparison instead.
func (r BlockedReason) recoverable() bool { return r == BlockedReasonTranscriptMissing }

// publishedState is the small local cache of what was last built for a
// session: the exact source bundle (so a later scan can detect "no
// meaningful change" without redownloading or reparsing published history),
// when that happened, and why the bundle is in the state it's in.
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
	return &publishedSnapshot{Bundle: p.Bundle, PublishedAt: p.LastPublished.PublishedAt}
}

func (s *LocalStore) publishedPath(archiveSessionID string) string {
	return filepath.Join(s.home, "published", archiveSessionID+".json")
}

// SavePublished records the outcome of a build/publish decision for a
// session, so the next scan can compare against it instead of rebuilding
// from scratch. See CacheStatus for what each status means for retry.
func (s *LocalStore) SavePublished(archiveSessionID string, bundle archive.SourceBundle, publishedAt time.Time, status CacheStatus, metadata ...[]byte) error {
	return s.savePublishedState(archiveSessionID, bundle, publishedAt, status, "", metadata, nil)
}

// SaveBlocked records a terminal capture gap for a session (see
// CacheStatusBlocked). bundle is what the next scan compares against and
// publishedAt is the last actual publish time, if any; the last published
// snapshot itself is preserved exactly as SavePublished preserves it. For a
// recoverable reason, deferred is hook evidence the block acknowledged and
// must hand back when it clears; it accumulates across saves of the same
// block and is dropped, having been handed back, by any other status.
func (s *LocalStore) SaveBlocked(archiveSessionID string, bundle archive.SourceBundle, publishedAt time.Time, reason BlockedReason, deferred ...archive.SupplementalEvidence) error {
	if reason == "" {
		return errors.New("blocked reason is required")
	}
	return s.savePublishedState(archiveSessionID, bundle, publishedAt, CacheStatusBlocked, reason, nil, deferred)
}

func (s *LocalStore) savePublishedState(archiveSessionID string, bundle archive.SourceBundle, publishedAt time.Time, status CacheStatus, reason BlockedReason, metadata [][]byte, deferred []archive.SupplementalEvidence) error {
	if !safeFileComponent(archiveSessionID) {
		return errors.New("archive session ID is not a safe file name component")
	}
	var last *publishedSnapshot
	var existing publishedState
	if err := local.Read(s.publishedPath(archiveSessionID), &existing); err == nil {
		last = existing.detachedLastPublished()
		if last == nil && existing.Status == CacheStatusPublished {
			copy := publishedSnapshot{Bundle: existing.Bundle, PublishedAt: existing.PublishedAt}
			last = &copy
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read published state %q: %w", archiveSessionID, err)
	}
	if status == CacheStatusPublished {
		// The candidate becoming the current bundle is exactly what was just
		// published, so the snapshot records only when, not a second copy.
		last = &publishedSnapshot{PublishedAt: publishedAt, SameAsBundle: true}
	}
	preBlock := existing.PreBlockStatus
	switch {
	case status != CacheStatusBlocked:
		preBlock = ""
	case existing.Status != CacheStatusBlocked:
		preBlock = existing.Status
	}
	var held []archive.SupplementalEvidence
	if status == CacheStatusBlocked && reason.recoverable() {
		// The same block continuing keeps what it already holds; a block
		// that replaces a settled state starts with only what arrived now.
		if existing.Status == CacheStatusBlocked && existing.BlockedReason == reason {
			held = existing.DeferredHookEvidence
		}
		held = archive.MergeSupplementalEvidence(held, deferred)
	}
	return local.Write(s.publishedPath(archiveSessionID), publishedState{Bundle: bundle, PublishedAt: publishedAt, Status: status, BlockedReason: reason, PreBlockStatus: preBlock, DeferredHookEvidence: held, LastPublished: last, MetadataBytes: publicationMetadata(existing.MetadataBytes, metadata)})
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
func (s *LocalStore) ClearRecoverableBlock(archiveSessionID string, now time.Time) (restored CacheStatus, replayed, cleared bool, err error) {
	var state publishedState
	err = local.Read(s.publishedPath(archiveSessionID), &state)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, fmt.Errorf("read published state %q: %w", archiveSessionID, err)
	}
	if state.Status != CacheStatusBlocked || !state.BlockedReason.recoverable() {
		return state.Status, false, false, nil
	}
	if len(state.DeferredHookEvidence) > 0 {
		if err := s.SaveRequest(archiveSessionID, "transcript-returned", now, state.DeferredHookEvidence...); err != nil {
			return "", false, false, fmt.Errorf("replay evidence held while blocked %q: %w", archiveSessionID, err)
		}
		replayed = true
	}
	if state.PreBlockStatus == "" || state.PreBlockStatus == CacheStatusBlocked {
		return state.Status, replayed, false, nil
	}
	restored = state.PreBlockStatus
	state.Status, state.BlockedReason, state.PreBlockStatus, state.DeferredHookEvidence = restored, "", "", nil
	if err := local.Write(s.publishedPath(archiveSessionID), state); err != nil {
		return "", replayed, false, fmt.Errorf("clear block %q: %w", archiveSessionID, err)
	}
	return restored, replayed, true, nil
}

// LoadBlocked reports whether a session is in CacheStatusBlocked and why.
func (s *LocalStore) LoadBlocked(archiveSessionID string) (BlockedReason, bool, error) {
	var state publishedState
	readErr := local.Read(s.publishedPath(archiveSessionID), &state)
	if errors.Is(readErr, os.ErrNotExist) {
		return "", false, nil
	}
	if readErr != nil {
		return "", false, fmt.Errorf("read published state %q: %w", archiveSessionID, readErr)
	}
	if state.Status != CacheStatusBlocked {
		return "", false, nil
	}
	return state.BlockedReason, true, nil
}

// LoadPublished returns the last cached bundle for a session, if any.
func (s *LocalStore) LoadPublished(archiveSessionID string) (bundle archive.SourceBundle, publishedAt time.Time, status CacheStatus, found bool, err error) {
	var state publishedState
	readErr := local.Read(s.publishedPath(archiveSessionID), &state)
	if errors.Is(readErr, os.ErrNotExist) {
		return archive.SourceBundle{}, time.Time{}, "", false, nil
	}
	if readErr != nil {
		return archive.SourceBundle{}, time.Time{}, "", false, fmt.Errorf("read published state %q: %w", archiveSessionID, readErr)
	}
	return state.Bundle, state.PublishedAt, state.Status, true, nil
}

// LoadLastPublished returns the most recent bundle actually made discoverable
// by remote metadata. It deliberately ignores a newer local-only candidate.
func (s *LocalStore) LoadLastPublished(archiveSessionID string) (bundle archive.SourceBundle, publishedAt time.Time, found bool, err error) {
	var state publishedState
	readErr := local.Read(s.publishedPath(archiveSessionID), &state)
	if errors.Is(readErr, os.ErrNotExist) {
		return archive.SourceBundle{}, time.Time{}, false, nil
	}
	if readErr != nil {
		return archive.SourceBundle{}, time.Time{}, false, fmt.Errorf("read published state %q: %w", archiveSessionID, readErr)
	}
	bundle, publishedAt, found = state.resolveLastPublished()
	return bundle, publishedAt, found, nil
}

// PendingPublication is one fully rendered publication transaction. Source
// and metadata bytes are persisted together before the first remote write, so
// every retry uses the same hash and timestamps even after process restart.
// Bundle remains available for change detection and future parser-only rebuilds.
type PendingPublication struct {
	MetadataOnly  bool                 `json:"metadata_only,omitempty"`
	Bundle        archive.SourceBundle `json:"bundle"`
	SourceKey     string               `json:"source_key"`
	MetadataKey   string               `json:"metadata_key"`
	SourceSHA256  string               `json:"source_sha256"`
	SourceBytes   []byte               `json:"source_bytes"`
	MetadataBytes []byte               `json:"metadata_bytes"`
	RequestToken  string               `json:"request_token,omitempty"`
	ReadyAt       time.Time            `json:"ready_at"`
	Attempted     bool                 `json:"attempted,omitempty"`
}

func (s *LocalStore) pendingPath(id string) string {
	return filepath.Join(s.home, "pending", id+".json")
}

func (s *LocalStore) SavePending(id string, pending PendingPublication) error {
	if !safeFileComponent(id) {
		return errors.New("archive session ID is not a safe file name component")
	}
	if pending.SourceKey == "" || pending.MetadataKey == "" || pending.SourceSHA256 == "" || len(pending.SourceBytes) == 0 || len(pending.MetadataBytes) == 0 {
		return errors.New("pending publication is incomplete")
	}
	return local.Write(s.pendingPath(id), pending)
}

func (s *LocalStore) LoadPending(id string) (PendingPublication, bool, error) {
	var pending PendingPublication
	err := local.Read(s.pendingPath(id), &pending)
	if errors.Is(err, os.ErrNotExist) {
		return PendingPublication{}, false, nil
	}
	if err != nil {
		return PendingPublication{}, false, fmt.Errorf("read pending publication %q: %w", id, err)
	}
	return pending, true, nil
}

// HasPending reports whether a publication is still outstanding for a session
// without decoding it. The pending file carries the compressed source bytes,
// so a stat is the only way to ask this question cheaply enough to ask it for
// every registered session on every pass.
func (s *LocalStore) HasPending(id string) (bool, error) {
	if !safeFileComponent(id) {
		return false, errors.New("archive session ID is not a safe file name component")
	}
	_, err := os.Stat(s.pendingPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat pending publication %q: %w", id, err)
	}
	return true, nil
}

func (s *LocalStore) RemovePending(id string) error {
	err := os.Remove(s.pendingPath(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove pending publication %q: %w", id, err)
	}
	return nil
}

// Status summarizes the collector's local state for a future `status`
// command. It never includes transcript content.
type Status struct {
	SessionIssues   map[string]string `json:"session_issues,omitempty"`
	LastScanAt      time.Time         `json:"last_scan_at"`
	LastPublishedAt time.Time         `json:"last_published_at,omitempty"`
	PendingCount    int               `json:"pending_count"`
	LastError       string            `json:"last_error,omitempty"`
}

func (s *LocalStore) statusPath() string { return filepath.Join(s.home, "status.json") }

// SaveStatus durably records the latest Status.
func (s *LocalStore) SaveStatus(status Status) error {
	return local.Write(s.statusPath(), status)
}

// LoadStatus returns the last saved Status, or the zero value if none exists
// yet.
func (s *LocalStore) LoadStatus() (Status, error) {
	var status Status
	err := local.Read(s.statusPath(), &status)
	if errors.Is(err, os.ErrNotExist) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, fmt.Errorf("read status: %w", err)
	}
	return status, nil
}

// SetScanPending journals work before scanning. A failed or interrupted update
// remains pending even when the previous published cache is still valid.
func (s *LocalStore) SetScanPending(id string, pending bool) error {
	if !safeFileComponent(id) {
		return errors.New("invalid session ID")
	}
	path := filepath.Join(s.home, "pending-scans", id+".json")
	if pending {
		return local.Write(path, true)
	}
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *LocalStore) ScanPending(id string) (bool, error) {
	if !safeFileComponent(id) {
		return false, errors.New("invalid session ID")
	}
	var pending bool
	err := local.Read(filepath.Join(s.home, "pending-scans", id+".json"), &pending)
	if os.IsNotExist(err) {
		return false, nil
	}
	return pending, err
}

// scanSignature is one session's "nothing to do" token: the exact size and
// nanosecond modification time of the transcript the last completed scan
// consumed, plus the versions that scan ran under. Its presence asserts that
// the scan ended settled — published, declined, or unchanged — and never
// blocked, so the next pass can skip the session on a matching stat alone.
//
// It lives in its own small file rather than inside the published cache
// because reading it has to stay cheap: the published cache holds a whole
// source bundle (hundreds of kilobytes), and decoding one per registered
// session per pass is precisely the cost this token exists to remove.
// Anything that invalidates the assertion removes the token (see
// removeScanSignature's callers).
type scanSignature struct {
	TranscriptSize  int64 `json:"transcript_size"`
	TranscriptMtime int64 `json:"transcript_mtime_unix_nano"`
	// The derivation versions are part of the signature: a parser, filter, or
	// adapter upgrade changes what an unchanged transcript would produce, so
	// it must re-scan rather than skip.
	ParserVersion  string `json:"parser_version"`
	FilterVersion  string `json:"filter_version"`
	AdapterVersion string `json:"adapter_version"`
	// SourceFormat is the format the scan actually produced, which decides
	// whether a stat is trustworthy evidence at all (see unchangedSinceLastScan).
	SourceFormat string `json:"source_format,omitempty"`
}

func (s *LocalStore) scanSignaturePath(id string) string {
	return filepath.Join(s.home, "scan-signatures", id+".json")
}

// saveScanSignature records the token, skipping the write (and its two fsyncs)
// when nothing about it changed.
func (s *LocalStore) saveScanSignature(id string, signature scanSignature) error {
	if !safeFileComponent(id) {
		return errors.New("archive session ID is not a safe file name component")
	}
	if existing, found, err := s.loadScanSignature(id); err != nil {
		return err
	} else if found && existing == signature {
		return nil
	}
	return local.Write(s.scanSignaturePath(id), signature)
}

func (s *LocalStore) loadScanSignature(id string) (scanSignature, bool, error) {
	if !safeFileComponent(id) {
		return scanSignature{}, false, errors.New("archive session ID is not a safe file name component")
	}
	var signature scanSignature
	err := local.Read(s.scanSignaturePath(id), &signature)
	if errors.Is(err, os.ErrNotExist) {
		return scanSignature{}, false, nil
	}
	if err != nil {
		// A corrupt token is not a failure: it only means this session cannot
		// be skipped, which is the safe answer.
		return scanSignature{}, false, nil
	}
	return signature, true, nil
}

func (s *LocalStore) removeScanSignature(id string) error {
	if !safeFileComponent(id) {
		return errors.New("archive session ID is not a safe file name component")
	}
	err := os.Remove(s.scanSignaturePath(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove scan signature %q: %w", id, err)
	}
	return nil
}

func publicationMetadata(previous []byte, supplied [][]byte) []byte {
	if len(supplied) > 0 {
		return supplied[0]
	}
	return previous
}
