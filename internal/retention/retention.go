// Package retention deletes local-machine-owned archive data that has aged
// out: source snapshots superseded more than a grace period ago, and,
// separately, entire sessions once their most recently captured evidence is
// older than a configured retention window.
//
// Before deleting anything it fetches the session's remote metadata.json
// and treats that, not the local cache, as the current pointer: the current
// source is never deleted regardless of age, and neither is its immediate
// predecessor (the most recently superseded snapshot), so a reader that
// just fetched metadata always has a snapshot to fall back to. Whole-session
// expiry deletes metadata before sources, so an interruption leaves at
// worst unreferenced objects, never a live pointer to missing data; a
// failed expiry keeps the local registration (ownership) so the next sweep
// retries it. It never considers another machine's sessions — registrations
// only ever exist on the machine that created them.
package retention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// Options configures one Sweep call.
type Options struct {
	// CurrentDestination reports whether a registration's published evidence
	// lives in the bucket this sweep is connected to. It deliberately is not
	// the collector's AcceptSession: that decides what may still be published,
	// and a session this machine has stopped publishing (its project was
	// excluded, its application deselected) still owns objects here that must
	// age out. Only a session admitted into another storage destination (by
	// its recorded destination ID, or for an older registration without one,
	// admitted before the current destination was configured) has its
	// objects somewhere else; its local state is still pruned when it
	// expires, but no delete is ever issued against the current bucket on
	// its behalf. Nil means every registration belongs to the current
	// destination.
	CurrentDestination func(archive.SessionRegistration) bool
	// Publishable is the collector's AcceptSession: whether this session's
	// outstanding work will ever be published. It never decides whether a
	// session is swept. It only decides whether a pending request or
	// publication defers expiry, because deferring for work the collector
	// will never do would keep that session forever. Nil means publishable.
	Publishable func(archive.SessionRegistration) bool
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
	// GracePeriod bounds how long a superseded (no longer current) source
	// snapshot stays downloadable before deletion. It applies only to
	// snapshots older than the immediate predecessor of the current source,
	// which is retained regardless of age. Defaults to 24h, matching the
	// spec's proposed grace period.
	GracePeriod time.Duration
	// SessionMaxAge is whole-session retention: once a session's most
	// recently captured evidence is older than this, its metadata and every
	// source snapshot are deleted together, never a source-only rule that
	// could leave a live metadata pointer dangling. Zero disables
	// whole-session deletion.
	SessionMaxAge time.Duration
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) gracePeriod() time.Duration {
	if o.GracePeriod > 0 {
		return o.GracePeriod
	}
	return 24 * time.Hour
}

// Result summarizes one Sweep call.
type Result struct {
	DeletedSnapshots int
	DeletedSessions  []string
	// PrunedSessions expired only locally, with no call to the current bucket:
	// nothing of theirs is in it, either because their evidence was published
	// to a destination this machine no longer uses or because nothing was
	// ever published (the transcript vanished before the first capture).
	PrunedSessions []string
	Errors         map[string]error
}

// Sweep processes every session registered on this machine, including ones the
// collector would no longer publish: a registration this machine owns is the
// only record that its remote objects exist, so skipping it would leave that
// session's data and local state behind forever. One session's failure is
// isolated in Result.Errors and left to retry on the next call, like
// collector.Run.
func Sweep(ctx context.Context, local *state.Store, store storage.ObjectStore, opts Options) (Result, error) {
	now := opts.now()
	result := Result{Errors: map[string]error{}}

	// An unreadable registration or request fails only its own session, as
	// in collector.Run.
	registrations, registrationIssues, err := local.ScanRegistrations()
	if err != nil {
		return Result{}, err
	}
	maps.Copy(result.Errors, registrationIssues)
	// One directory read for the whole sweep, rather than one per session.
	requests, requestIssues, err := local.ScanRequests()
	if err != nil {
		return Result{}, err
	}
	requested := make(map[string]bool, len(requests))
	for _, req := range requests {
		requested[req.ArchiveSessionID] = true
	}
	for id, issue := range requestIssues {
		result.Errors[id] = issue
		// A request that could not be read may still hold evidence, so it
		// defers expiry like any other; a quarantined one no longer exists.
		if !errors.Is(issue, state.ErrQuarantined) {
			requested[id] = true
		}
	}
	for _, reg := range registrations {
		if err := sweepSession(ctx, local, store, reg, opts, now, requested, &result); err != nil {
			result.Errors[reg.ArchiveSessionID] = err
		}
	}
	return result, nil
}

func sweepSession(ctx context.Context, local *state.Store, store storage.ObjectStore, reg archive.SessionRegistration, opts Options, now time.Time, requested map[string]bool, result *Result) error {
	bundle, _, _, found, err := local.LoadPublished(reg.ArchiveSessionID)
	if err != nil {
		return fmt.Errorf("load published cache: %w", err)
	}

	// A session ages from its cached capture when it has one. A registration
	// that never produced a capture (the transcript vanished before the first
	// scan, or was never readable) ages from its admission instead; otherwise
	// it would be the one kind of local state that never expires. Admission,
	// not the start: an imported session can have started years ago, and a
	// failed first upload must not expire it at once.
	ageFrom := reg.Admitted()
	if found && !bundle.Capture.CapturedAt.IsZero() {
		ageFrom = bundle.Capture.CapturedAt
	}
	locallyExpired := opts.SessionMaxAge > 0 && !ageFrom.IsZero() && now.Sub(ageFrom) >= opts.SessionMaxAge
	// deferForWork says whether a queued request or pending publication
	// postpones expiry: only when the collector will actually do that work.
	// A session it no longer publishes is one case. A registration that never
	// received a transcript path is the other: Cursor registers a chat at its
	// first prompt and the path normally arrives with a later hook, but with
	// the app's transcripts disabled it never does, so nothing can ever be
	// captured for it. Once such a session is itself older than the retention
	// window, its request (which can carry hook text) stops deferring expiry;
	// otherwise the registration and that text would stay on this machine
	// forever. It never published, so forgetting it needs no bucket call. A
	// session read from Cursor's database has no path by design and is
	// captured all the same, so its work still defers expiry.
	deferForWork := opts.Publishable == nil || opts.Publishable(reg)
	if admitted := reg.Admitted(); reg.ReadsTranscriptFile() && reg.TranscriptPath == "" && opts.SessionMaxAge > 0 && !admitted.IsZero() && now.Sub(admitted) >= opts.SessionMaxAge {
		deferForWork = false
	}
	if locallyExpired && deferForWork {
		// Unpublished work is not expired evidence. A session that crosses the
		// retention boundary on the same pass its publication fails
		// transiently would otherwise have its new source, its pending
		// publication, its request, and that request's hook evidence all
		// deleted together, silently. Say nothing and retry next pass: either
		// the publication lands and the session expires with its evidence
		// intact, or it keeps failing and reports itself as a failure. A
		// session the collector no longer publishes is not deferred: its
		// outstanding work will never be done, so waiting on it would keep
		// the session forever.
		unfinished, err := hasUnfinishedWork(local, requested, reg.ArchiveSessionID)
		if err != nil {
			return err
		}
		locallyExpired = !unfinished
	}

	// A session admitted into another destination has no objects in this
	// bucket. Its local state still ages
	// out; nothing is deleted remotely, here or in the bucket it came from.
	if opts.CurrentDestination != nil && !opts.CurrentDestination(reg) {
		if !locallyExpired {
			return nil
		}
		forgotten, err := forgetExpired(local, reg, deferForWork, now)
		if forgotten {
			result.PrunedSessions = append(result.PrunedSessions, reg.ArchiveSessionID)
		}
		if err != nil {
			return fmt.Errorf("forget session from a previous destination: %w", err)
		}
		return nil
	}

	if locallyExpired {
		// Nothing of this session can be in the bucket unless a publication
		// was recorded, or one is still pending: a pending upload may have
		// reached storage before its local acknowledgement did. Anything
		// else is local state only, so it is forgotten without a remote call.
		_, _, everPublished, err := local.LoadLastPublished(reg.ArchiveSessionID)
		if err != nil {
			return fmt.Errorf("load last published bundle: %w", err)
		}
		pending, err := local.HasPending(reg.ArchiveSessionID)
		if err != nil {
			return fmt.Errorf("check pending publication: %w", err)
		}
		if !everPublished && !pending {
			forgotten, err := forgetExpired(local, reg, deferForWork, now)
			if forgotten {
				result.PrunedSessions = append(result.PrunedSessions, reg.ArchiveSessionID)
			}
			if err != nil {
				return fmt.Errorf("forget never-published session: %w", err)
			}
			return nil
		}
	}

	superseded, err := local.LoadSuperseded(reg.ArchiveSessionID)
	if err != nil {
		return fmt.Errorf("load superseded sources: %w", err)
	}
	// Skip the remote round trip when this pass could not delete anything:
	// the predecessor stays in the ledger forever, so without this every
	// session ever republished would cost one GET per sync indefinitely.
	if !locallyExpired && !anySupersededExpirable(superseded, now, opts.gracePeriod()) {
		return nil
	}

	metadataKey, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if err != nil {
		return err
	}
	data, remoteErr := store.Get(ctx, metadataKey)
	var metadata archive.Metadata
	if remoteErr == nil {
		if err := json.Unmarshal(data, &metadata); err != nil {
			return fmt.Errorf("decode current metadata: %w", err)
		}
		if err := metadata.ValidateSourceReference(); err != nil {
			return fmt.Errorf("invalid current metadata: %w", err)
		}
		if metadata.SessionID != reg.ArchiveSessionID || metadata.Harness.Name != reg.Harness.Name || !strings.HasPrefix(metadata.SourceBundle.Key, fmt.Sprintf("sessions/%s/%s/", reg.Harness.Name, reg.ArchiveSessionID)) {
			return fmt.Errorf("current metadata belongs to another session")
		}
	} else if !errors.Is(remoteErr, storage.ErrNotFound) {
		return fmt.Errorf("read current metadata before cleanup: %w", remoteErr)
	}
	// Protect evidence published remotely just before a local acknowledgement failed.
	capturedAt := ageFrom
	if remoteErr == nil && metadata.CapturedAt.After(capturedAt) {
		capturedAt = metadata.CapturedAt
	}

	// locallyExpired already carries SessionMaxAge, the cached capture time,
	// and the unfinished-work deferral; remote metadata can only make
	// capturedAt later, so it can only withdraw expiry, never grant it.
	if locallyExpired && !capturedAt.IsZero() && now.Sub(capturedAt) >= opts.SessionMaxAge {
		if err := collector.DeleteWholeSession(ctx, store, reg.Harness.Name, reg.ArchiveSessionID); err != nil {
			return fmt.Errorf("delete session: %w", err)
		}
		// The remote deletion takes network time and is not done under the
		// request lock, because a hook waits for that lock on the user's
		// turn. A hook that fired meanwhile has left a request, and the
		// locked recheck keeps the session for it. The next collector pass
		// then publishes a complete bundle carrying that evidence (every
		// publication is the whole session, not a delta), or, if the request
		// added nothing, acknowledges it and the next sweep finishes the
		// expiry.
		forgotten, err := forgetExpired(local, reg, deferForWork, now)
		if forgotten {
			result.DeletedSessions = append(result.DeletedSessions, reg.ArchiveSessionID)
		}
		if err != nil {
			return fmt.Errorf("forget session: %w", err)
		}
		return nil
	}

	if len(superseded) == 0 {
		return nil
	}

	if remoteErr != nil {
		return fmt.Errorf("current metadata is missing; preserve superseded sources")
	}
	currentKey := metadata.SourceBundle.Key
	// Append order records supersession order even if the clock moves backward.
	var predecessorKey string
	for _, s := range superseded {
		if !strings.HasPrefix(s.Key, fmt.Sprintf("sessions/%s/%s/source.", reg.Harness.Name, reg.ArchiveSessionID)) {
			return fmt.Errorf("superseded source belongs to another session")
		}
		if s.Key != currentKey {
			predecessorKey = s.Key
		}
	}
	for _, s := range superseded {
		if s.Key == currentKey {
			// Defensive: a key must never be both current and superseded;
			// if it somehow is, trust "current" and just clean the ledger.
			if err := local.RemoveSuperseded(reg.ArchiveSessionID, s.Key); err != nil {
				return fmt.Errorf("clean up ledger entry: %w", err)
			}
			continue
		}
		if s.Key == predecessorKey || now.Sub(s.SupersededAt) < opts.gracePeriod() {
			continue
		}
		if err := store.Delete(ctx, s.Key); err != nil {
			return fmt.Errorf("delete superseded source %q: %w", s.Key, err)
		}
		if err := local.RemoveSuperseded(reg.ArchiveSessionID, s.Key); err != nil {
			return fmt.Errorf("update superseded ledger: %w", err)
		}
		result.DeletedSnapshots++
	}
	return nil
}

// forgetExpired forgets an expired session and leaves a removal record so
// backfill does not import it again. Every path that calls it is
// whole-session expiry past the retention window: after deleting the session
// from the current bucket, for a session that never published, and for one
// published to a previous destination. ForgetIdleSession writes the record
// under the request lock, so a session a hook kept alive gets none, and a
// failed write keeps the session registered for the next sweep to retry.
func forgetExpired(local *state.Store, reg archive.SessionRegistration, deferForWork bool, now time.Time) (bool, error) {
	return local.ForgetIdleSession(reg.ArchiveSessionID, reg.NativeSessionID, deferForWork, &state.RemovalRecord{
		Harness: reg.Harness.Name, Reason: state.RemovalReasonRetention, At: now,
	})
}

// hasUnfinishedWork reports whether the collector still owes this session a
// publication: a request a hook left behind, or a publication built and not
// yet accepted by storage. Either one means evidence exists that expiry would
// destroy before it was ever archived.
func hasUnfinishedWork(local *state.Store, requested map[string]bool, archiveSessionID string) (bool, error) {
	if requested[archiveSessionID] {
		return true, nil
	}
	pending, err := local.HasPending(archiveSessionID)
	if err != nil {
		return false, fmt.Errorf("check pending publication: %w", err)
	}
	return pending, nil
}

// anySupersededExpirable reports whether a sweep could delete at least one
// ledger entry now. The last entry is either the current source or its
// immediate predecessor, and neither is ever deleted, so only an earlier
// entry past its grace period can be. This is a conservative necessary
// condition: it may say yes for an entry the full check then retains, but
// never no when a deletion is possible, so skipping on false is safe.
func anySupersededExpirable(superseded []state.SupersededSource, now time.Time, grace time.Duration) bool {
	for i, s := range superseded {
		if i == len(superseded)-1 {
			return false
		}
		if now.Sub(s.SupersededAt) >= grace {
			return true
		}
	}
	return false
}
