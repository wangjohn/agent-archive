// Package state is agent-archive's per-machine local state: the small files
// under the private data directory (local.Home) that hooks, the collector,
// retention, backfill, and the CLI share. It owns their layout, the
// per-session locks that keep a hook and the collector from interleaving,
// and every rule about what a file may hold:
//
//   - registrations/: one per session a hook (or backfill) admitted;
//   - requests/: hook evidence waiting for the next collector pass;
//   - published/: the last bundle built and the last one published, with
//     the uploaded source reference and metadata (see Published);
//   - pending/: a publication transaction frozen before its first upload;
//   - scan-signatures/, pending-scans/: the collector's change detection;
//   - superseded/: source objects retention may delete after a grace period;
//   - sessions/ (the native-session index and per-session evidence),
//     subagent-candidates/, forgotten/, refresh-skips/, and status.json.
//
// It never reads transcripts or talks to storage; the collector does both
// and records the outcome here. It holds no credentials and no content
// beyond what published bundles already carry. A Store is safe for the
// processes that share a data directory as long as each follows the
// documented locks; the collector lock (local.Lock) additionally makes the
// collector the only writer of published/ and pending/ during a pass.
package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/local"
)

// Store persists small operational files under a private home
// directory (see local.Home): registrations, upload requests, and a
// per-session cache of the last published source bundle, used to detect
// unchanged input without redownloading or reparsing published history. It
// never stores credentials or a second copy of conversation content beyond
// what the published source bundle itself already contains.
type Store struct {
	home string
}

// OpenReadOnly returns a handle to an existing local store under
// home without creating any of its directories, for commands that only read
// it (status, handoff, backfill planning). A missing directory reads as
// nothing recorded.
func OpenReadOnly(home string) *Store { return &Store{home: home} }

// Open creates (if needed) the local store's directory layout under
// home — ordinarily the result of local.Home() — and returns a handle to it.
// home is caller-owned; this package never deletes it.
func Open(home string) (*Store, error) {
	if strings.TrimSpace(home) == "" {
		return nil, errors.New("local store home is required")
	}
	for _, dir := range storeDirs {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
			return nil, fmt.Errorf("create local store directory %q: %w", dir, err)
		}
	}
	return &Store{home: home}, nil
}

// Home returns the data directory the store keeps its files in.
func (s *Store) Home() string { return s.home }

// storeDirs are the directories Open creates under home.
var storeDirs = []string{"registrations", "requests", "request-locks", "published", "pending", "sessions", "pending-scans", "scan-signatures", "subagent-candidates"}

// lazyStoreDirs are the directories the store creates under home on first
// use rather than up front.
var lazyStoreDirs = []string{"superseded", "forgotten", refreshSkipDir}

// OwnedEntries lists every top-level entry a Store can create under its
// home: its directories and its status file. Uninstall deletes a data
// directory entry by entry and must know all of them; a test there checks
// its list against this one, so a new directory cannot be left behind.
func OwnedEntries() []string {
	entries := append(append([]string{}, storeDirs...), lazyStoreDirs...)
	return append(entries, "status.json")
}

func safeFileComponent(value string) bool {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\") {
		return false
	}
	return true
}

// SaveRegistration durably records a hook-observed session start. Re-saving
// the same archive session ID overwrites its prior registration.
func (s *Store) SaveRegistration(reg archive.SessionRegistration) error {
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
func (s *Store) UpdateRegistration(archiveSessionID string, update func(*archive.SessionRegistration) error) (found bool, err error) {
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
func (s *Store) RegisterNewSession(nativeSessionID string, build func(archiveSessionID string) archive.SessionRegistration) (archive.SessionRegistration, error) {
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

func (s *Store) registerUnderLock(nativeSessionID, id string, build func(string) archive.SessionRegistration) (archive.SessionRegistration, bool, error) {
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

func (s *Store) registrationPath(archiveSessionID string) string {
	return filepath.Join(s.home, "registrations", archiveSessionID+".json")
}

// LoadRegistrations returns every registered session, sorted by archive
// session ID for deterministic scan order.
func (s *Store) LoadRegistrations() ([]archive.SessionRegistration, error) {
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
			if errors.Is(err, os.ErrNotExist) {
				// Forgotten between listing and reading.
				continue
			}
			return nil, fmt.Errorf("read registration %q: %w", entry.Name(), err)
		}
		out = append(out, reg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ArchiveSessionID < out[j].ArchiveSessionID })
	return out, nil
}

// LoadRegistration returns one session's registration, if it has been saved.
func (s *Store) LoadRegistration(archiveSessionID string) (archive.SessionRegistration, bool, error) {
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

// Urgent reports whether the request asks the collector to flush the upload
// debounce now rather than wait for the next scheduled publication.
func (r Request) Urgent() bool {
	return r.Token != "" && !r.Deferred
}

// SaveRequest merges a new hook event into any already-pending request for
// the same session: reasons accumulate, evidence appends, and RequestedAt
// advances to the latest event. This is the coalescing the spec asks for
// when a stop and a session-end hook both fire for the same session. The
// resulting request is urgent: the collector publishes it as soon as it is
// scanned, bypassing MinUploadInterval.
func (s *Store) SaveRequest(archiveSessionID, reason string, requestedAt time.Time, evidence ...archive.SupplementalEvidence) error {
	return s.saveRequest(archiveSessionID, reason, requestedAt, false, evidence...)
}

// SaveEvidence appends lifecycle evidence to the session's request without
// asking for a flush. A request it creates is deferred; a request that is
// already pending keeps its urgency and RequestedAt, so a stop that is
// waiting to publish is neither delayed nor re-triggered by a later prompt.
func (s *Store) SaveEvidence(archiveSessionID, reason string, observedAt time.Time, evidence ...archive.SupplementalEvidence) error {
	return s.saveRequest(archiveSessionID, reason, observedAt, true, evidence...)
}

// ErrSessionNotRegistered is returned when a request is written for a session
// whose registration no longer exists, typically because retention forgot it
// between the caller's own lookup and the write.
var ErrSessionNotRegistered = errors.New("session is no longer registered")

// lockRequest takes the per-session request lock. Hooks writing a request,
// the collector acknowledging one, and retention forgetting the session all
// hold it, so none of them can interleave with another.
func (s *Store) lockRequest(archiveSessionID string) (func(), error) {
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
func (s *Store) saveRequest(archiveSessionID, reason string, requestedAt time.Time, deferred bool, evidence ...archive.SupplementalEvidence) error {
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
	existing, found, err := s.LoadRequest(archiveSessionID)
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

func (s *Store) requestPath(archiveSessionID string) string {
	return filepath.Join(s.home, "requests", archiveSessionID+".json")
}

// LoadRequest returns a session's pending request, if any. Unlike ScanRequests
// it never moves a file aside, so hooks may call it.
func (s *Store) LoadRequest(archiveSessionID string) (Request, bool, error) {
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

// EnsureRequestToken upgrades a request written by an older collector. The
// token is assigned under the same lock used by hooks and acknowledgements so
// migration cannot overwrite a concurrent hook update. found is false when
// the request disappeared between listing and upgrade.
func (s *Store) EnsureRequestToken(archiveSessionID string) (Request, bool, error) {
	unlock, err := local.NamedLockWait(s.home, requestLockName(archiveSessionID), time.Second)
	if err != nil {
		return Request{}, false, fmt.Errorf("lock request %q: %w", archiveSessionID, err)
	}
	defer unlock()
	request, found, err := s.LoadRequest(archiveSessionID)
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
func (s *Store) LoadRequests() ([]Request, error) {
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
		req, found, err := s.LoadRequest(id)
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
func (s *Store) CompleteRequest(archiveSessionID, coveredToken string) (bool, error) {
	if !safeFileComponent(archiveSessionID) {
		return false, errors.New("archive session ID is not a safe file name component")
	}
	unlock, err := local.NamedLockWait(s.home, requestLockName(archiveSessionID), time.Second)
	if err != nil {
		return false, fmt.Errorf("lock request %q: %w", archiveSessionID, err)
	}
	defer unlock()
	current, found, err := s.LoadRequest(archiveSessionID)
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

// PendingPublication is one fully rendered publication transaction. Source
// and metadata bytes are persisted together before the first remote write, so
// every retry uses the same hash and timestamps even after process restart.
// Bundle remains available for change detection and future parser-only rebuilds.
type PendingPublication struct {
	MetadataOnly bool                 `json:"metadata_only,omitempty"`
	Bundle       archive.SourceBundle `json:"bundle"`
	SourceKey    string               `json:"source_key"`
	MetadataKey  string               `json:"metadata_key"`
	SourceSHA256 string               `json:"source_sha256"`
	SourceBytes  []byte               `json:"source_bytes"`
	// SourceSize is the source's compressed size when SourceBytes is empty:
	// a metadata-only publication over a source this build cannot reproduce
	// byte for byte, which is checked in storage instead of re-uploaded.
	SourceSize    int       `json:"source_size,omitempty"`
	MetadataBytes []byte    `json:"metadata_bytes"`
	RequestToken  string    `json:"request_token,omitempty"`
	ReadyAt       time.Time `json:"ready_at"`
	Attempted     bool      `json:"attempted,omitempty"`
}

// SourceReference is the reference the publication's metadata carries for
// its source object.
func (p PendingPublication) SourceReference() archive.SourceReference {
	size := len(p.SourceBytes)
	if p.CarriesNoSource() {
		size = p.SourceSize
	}
	return archive.SourceReference{Key: p.SourceKey, SHA256: p.SourceSHA256, CompressedBytes: size}
}

// CarriesNoSource reports a metadata-only publication that points at an
// existing source without carrying its bytes (see SourceSize).
func (p PendingPublication) CarriesNoSource() bool {
	return p.MetadataOnly && len(p.SourceBytes) == 0
}

func (s *Store) pendingPath(id string) string {
	return filepath.Join(s.home, "pending", id+".json")
}

// SavePending durably records a session's publication transaction before its
// first remote write. It refuses an incomplete one: every retry must upload
// exactly the same bytes under exactly the same keys.
func (s *Store) SavePending(id string, pending PendingPublication) error {
	if !safeFileComponent(id) {
		return errors.New("archive session ID is not a safe file name component")
	}
	if pending.SourceKey == "" || pending.MetadataKey == "" || pending.SourceSHA256 == "" || len(pending.MetadataBytes) == 0 || (len(pending.SourceBytes) == 0 && (!pending.MetadataOnly || pending.SourceSize <= 0)) {
		return errors.New("pending publication is incomplete")
	}
	return local.Write(s.pendingPath(id), pending)
}

// LoadPending returns a session's outstanding publication transaction, if
// any. Decoding it reads the whole compressed source; HasPending answers
// whether one exists without that cost.
func (s *Store) LoadPending(id string) (PendingPublication, bool, error) {
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
func (s *Store) HasPending(id string) (bool, error) {
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

// RemovePending discards a session's publication transaction once it has
// been published and acknowledged locally. A missing one is not an error.
func (s *Store) RemovePending(id string) error {
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
	LastScanAt      time.Time         `json:"last_scan_at,omitzero"`
	LastPublishedAt time.Time         `json:"last_published_at,omitzero"`
	PendingCount    int               `json:"pending_count"`
	LastError       string            `json:"last_error,omitempty"`
	// QuarantinedFiles lists, relative to the archive directory, the local
	// state files a pass found undecodable and moved aside (see
	// ErrQuarantined). They stay listed until someone inspects and deletes
	// them.
	QuarantinedFiles []string `json:"quarantined_files,omitempty"`
	// UnrefreshableSummaries counts the sessions whose published metadata
	// the current parser cannot refresh: the retained bundle cannot be
	// read by this build, or its recorded source is gone from storage and
	// cannot be rebuilt. Their metadata stays as published until the
	// session changes. It names no session and no content.
	UnrefreshableSummaries int `json:"unrefreshable_summaries,omitempty"`
}

func (s *Store) statusPath() string { return filepath.Join(s.home, "status.json") }

// SaveStatus durably records the latest Status.
func (s *Store) SaveStatus(status Status) error {
	return local.Write(s.statusPath(), status)
}

// LoadStatus returns the last saved Status, or the zero value if none exists
// yet.
func (s *Store) LoadStatus() (Status, error) {
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
func (s *Store) SetScanPending(id string, pending bool) error {
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

// ScanPending reports whether a scan of the session was journaled by
// SetScanPending and never completed, so its outcome is still owed.
func (s *Store) ScanPending(id string) (bool, error) {
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

// ScanSignature is one session's "nothing to do" token: the exact size and
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
// RemoveScanSignature's callers).
type ScanSignature struct {
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
	// SourceKind is the registration's source. A Cursor database chat is
	// identified by its cursorstore.Signature instead of a file stat.
	SourceKind            archive.SourceKind `json:"source_kind,omitempty"`
	CursorLastUpdatedAt   int64              `json:"cursor_last_updated_at,omitempty"`
	CursorHeaderCount     int                `json:"cursor_header_count,omitempty"`
	CursorLastBubbleID    string             `json:"cursor_last_bubble_id,omitempty"`
	CursorMessageRows     int                `json:"cursor_message_rows,omitempty"`
	CursorLastMessageHash string             `json:"cursor_last_message_hash,omitempty"`
	// Failed marks a Cursor chat whose read at this state could not be
	// captured (see rememberFailedRead); FailedError is that error's text,
	// "" for a recorded gap.
	Failed      bool   `json:"failed,omitempty"`
	FailedError string `json:"failed_error,omitempty"`
	// FailedMaxBytes and FailedRecordLimit are the size limits the failed
	// read ran under; the failure stands only while they do.
	FailedMaxBytes    int64 `json:"failed_max_bytes,omitempty"`
	FailedRecordLimit int64 `json:"failed_record_limit,omitempty"`
}

// CursorSignature is the Cursor chat state the signature was recorded at.
func (s ScanSignature) CursorSignature() cursorstore.Signature {
	return cursorstore.Signature{
		LastUpdatedAt: s.CursorLastUpdatedAt, HeaderCount: s.CursorHeaderCount, LastBubbleID: s.CursorLastBubbleID,
		MessageRows: s.CursorMessageRows, LastMessageHash: s.CursorLastMessageHash,
	}
}

func (s *Store) scanSignaturePath(id string) string {
	return filepath.Join(s.home, "scan-signatures", id+".json")
}

// SaveScanSignature records the token, skipping the write (and its two fsyncs)
// when nothing about it changed.
func (s *Store) SaveScanSignature(id string, signature ScanSignature) error {
	if !safeFileComponent(id) {
		return errors.New("archive session ID is not a safe file name component")
	}
	if existing, found, err := s.LoadScanSignature(id); err != nil {
		return err
	} else if found && existing == signature {
		return nil
	}
	return local.Write(s.scanSignaturePath(id), signature)
}

// LoadScanSignature returns a session's scan signature. One that cannot be
// read counts as absent: the session is then scanned, the safe answer.
func (s *Store) LoadScanSignature(id string) (ScanSignature, bool, error) {
	if !safeFileComponent(id) {
		return ScanSignature{}, false, errors.New("archive session ID is not a safe file name component")
	}
	var signature ScanSignature
	err := local.Read(s.scanSignaturePath(id), &signature)
	if errors.Is(err, os.ErrNotExist) {
		return ScanSignature{}, false, nil
	}
	if err != nil {
		// A corrupt token is not a failure: it only means this session cannot
		// be skipped, which is the safe answer.
		return ScanSignature{}, false, nil
	}
	return signature, true, nil
}

// RemoveScanSignature drops a session's scan signature, so the next pass
// scans it rather than skipping it on a stat.
func (s *Store) RemoveScanSignature(id string) error {
	if !safeFileComponent(id) {
		return errors.New("archive session ID is not a safe file name component")
	}
	err := os.Remove(s.scanSignaturePath(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove scan signature %q: %w", id, err)
	}
	return nil
}
