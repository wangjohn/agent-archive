package retention

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// Every time retention compares was stamped by this Mac's clock: a capture's
// CapturedAt, a ledger entry's SupersededAt, a registration's admission, and
// Now itself. The remote metadata offers no second opinion, since the same
// clock stamped its CapturedAt, and storage refuses nothing either: the AWS
// SDK corrects for clock skew on its own. So a clock set a year ahead would
// otherwise expire every session in the bucket within one pass.
//
// Nothing is therefore deleted, remotely or locally, because of age unless two
// independent sources agree that the clock is right:
//
//   - the storage service's own clock (Options.ServerClock) is no more than
//     MaxClockSkew behind Now; and
//   - Now is no more than MaxPassGap past the previous collector pass
//     (Options.PreviousScanAt). This one holds for a single pass only, and
//     is there for a storage service that shares this Mac's clock (MinIO
//     on the same machine), which the first check cannot catch.
//
// When either disagrees the sweep holds every such deletion (Result.Held) and
// deletes nothing on account of age; the check runs at most once a sweep, and
// only when a deletion is about to happen; a recent reading of the storage
// clock stands in for a new probe (see cachedProbe). A clock that is behind
// only delays deletions, the safe direction, and is let through.
//
// The mirror case is evidence stamped while the clock was ahead and swept
// once it is right again: a capture time or supersession time in the future.
// Left alone it would keep that evidence past retention until the date came
// round. Once the storage service confirms the clock is right in both
// directions, such a time is clamped durably to now: aged from now, not
// deleted early.

var (
	// ErrClockAhead holds age-driven deletion while this Mac's clock is ahead
	// of the storage service's by more than MaxClockSkew.
	ErrClockAhead = errors.New("this Mac's clock is ahead of the storage service's; retention deletes nothing until it is corrected")
	// ErrClockUnverified holds age-driven deletion when the storage service's
	// clock could not be read.
	ErrClockUnverified = errors.New("the storage service's clock could not be read to confirm this Mac's; retention deletes nothing until it can be")
	// ErrClockJumped holds age-driven deletion for one pass when the clock is
	// far past the previous collector pass: it jumped, or the Mac was off.
	ErrClockJumped = errors.New("this Mac's clock moved far past the previous collector pass; retention waits one pass")
)

const (
	// MaxClockSkew is how far Now may be ahead of the storage service's clock
	// before age-driven deletion is held, and how close the two must be
	// before a future time stamped by a clock that was ahead is clamped.
	MaxClockSkew = time.Hour
	// MaxPassGap is how far Now may be past the previous collector pass
	// before age-driven deletion waits a pass.
	MaxPassGap = 24 * time.Hour
)

// clockVerdict is a sweep's one reading of its clock.
type clockVerdict struct {
	checked bool
	// held is why age-driven deletion is held, or nil.
	held error
	// agrees reports that Now is within MaxClockSkew of the storage
	// service's clock in both directions, which clamping a future time needs.
	agrees bool
}

// readClock reaches the sweep's clock verdict, once.
func (s *sweeper) readClock() clockVerdict {
	if s.clock.checked {
		return s.clock
	}
	s.clock.checked = true
	serverClock := s.opts.ServerClock
	if serverClock == nil {
		serverClock = s.cachedProbe
	}
	serverNow, err := serverClock(s.ctx)
	switch {
	case err != nil:
		s.clock.held = fmt.Errorf("%w: %w", ErrClockUnverified, err)
	case s.now.Sub(serverNow) > MaxClockSkew:
		s.clock.held = fmt.Errorf("%w (by %s)", ErrClockAhead, roundedDuration(s.now.Sub(serverNow)))
	default:
		s.clock.agrees = serverNow.Sub(s.now) <= MaxClockSkew
	}
	// The storage clock's verdict comes first, so a clock that is ahead is
	// reported as that rather than as a one-pass wait.
	if previous := s.opts.PreviousScanAt; s.clock.held == nil && !previous.IsZero() && s.now.Sub(previous) > MaxPassGap {
		s.clock.held = fmt.Errorf("%w (%s since it)", ErrClockJumped, roundedDuration(s.now.Sub(previous)))
		s.clock.agrees = false
	}
	return s.clock
}

// clockAllowsDeletion reports whether a deletion decided by age may happen
// this sweep, and records the hold when it may not.
func (s *sweeper) clockAllowsDeletion() bool {
	verdict := s.readClock()
	if verdict.held != nil {
		s.result.Held = verdict.held
		return false
	}
	return true
}

// clockAgrees reports whether the clock is confirmed right in both
// directions, as clamping a future time needs.
func (s *sweeper) clockAgrees() bool {
	verdict := s.readClock()
	return verdict.held == nil && verdict.agrees
}

// captureAge is the time a session with a cached capture ages from. A capture
// stamped in the future by a clock that was ahead is clamped durably to now
// once the clock is confirmed right (see the mirror case above); until then
// it stands, which only keeps the session longer.
func (s *sweeper) captureAge(id string, summary state.PublishedSummary) (time.Time, error) {
	ageFrom := summary.RetentionAge()
	if !ageFrom.After(s.now.Add(MaxClockSkew)) || !s.clockAgrees() {
		return ageFrom, nil
	}
	published, err := s.local.LoadPublishedState(id)
	if err != nil {
		return time.Time{}, fmt.Errorf("load published cache: %w", err)
	}
	if err := published.ClampAgeFrom(s.now); err != nil {
		return time.Time{}, fmt.Errorf("clamp a capture time from the future: %w", err)
	}
	return s.now, nil
}

// supersededLedger reads a session's superseded-source ledger, clamping an
// entry stamped in the future once the clock is confirmed right.
func (s *sweeper) supersededLedger(id string) ([]state.SupersededSource, error) {
	superseded, err := s.local.LoadSuperseded(id)
	if err != nil {
		return nil, fmt.Errorf("load superseded sources: %w", err)
	}
	future := false
	for _, entry := range superseded {
		future = future || entry.SupersededAt.After(s.now.Add(MaxClockSkew))
	}
	if !future || !s.clockAgrees() {
		return superseded, nil
	}
	if err := s.local.ClampSuperseded(id, s.now); err != nil {
		return nil, fmt.Errorf("clamp a supersession time from the future: %w", err)
	}
	superseded, err = s.local.LoadSuperseded(id)
	if err != nil {
		return nil, fmt.Errorf("load superseded sources: %w", err)
	}
	return superseded, nil
}

const (
	// agreeingReadingReuse is how long a reading that let deletion through
	// stands in for a new one. The estimate it gives (the reading's server
	// time plus the time this Mac says has passed) misses a forward jump of
	// this Mac's clock inside the window, so the window is far shorter than
	// MaxClockSkew: a jump it can miss is too small to matter. A jump past
	// the window, or any jump backward, reads the clock again.
	agreeingReadingReuse = 10 * time.Minute
	// holdingReadingReuse is how long a reading that holds deletion (the
	// probe failed, or this Mac was ahead) stands in for a new one. Reusing
	// it can only hold deletion longer, never allow one, so it may stand
	// longer: a clock that stays wrong costs a probe an hour, not one a pass.
	holdingReadingReuse = time.Hour
)

// cachedProbe is the default ServerClock: the storage service's time from
// the last reading while that may still stand in for a new one (see
// agreeingReadingReuse and holdingReadingReuse), otherwise from a new probe,
// which is then recorded. Without it, a sweep that keeps finding something
// due while deletion is held (a clock that stays ahead, a probe that keeps
// failing) would write, list, and delete a probe object on every pass: in a
// versioned bucket, two versions a minute kept forever.
func (s *sweeper) cachedProbe(ctx context.Context) (time.Time, error) {
	if reading, found := s.local.LoadStorageClockReading(); found {
		if serverNow, ok, err := reuseReading(reading, s.now); ok {
			return serverNow, err
		}
	}
	serverNow, err := ProbeServerClock(ctx, s.store)
	if ctx.Err() != nil {
		// The sweep ran out of time; that says nothing about the clock.
		return serverNow, err
	}
	reading := state.StorageClockReading{CheckedAt: s.now.UTC(), ServerAt: serverNow.UTC()}
	if err != nil {
		reading = state.StorageClockReading{CheckedAt: s.now.UTC(), Error: err.Error()}
	}
	// Best effort: without the record the next sweep only probes again.
	_ = s.local.SaveStorageClockReading(reading)
	return serverNow, err
}

// reuseReading answers from an earlier reading when it may stand in for a
// new one at now: ok is false when the clock must be read again.
func reuseReading(reading state.StorageClockReading, now time.Time) (serverNow time.Time, ok bool, err error) {
	elapsed := now.Sub(reading.CheckedAt)
	if elapsed < 0 {
		// This Mac's clock went back since: read again.
		return time.Time{}, false, nil
	}
	if reading.Error != "" {
		if elapsed >= holdingReadingReuse {
			return time.Time{}, false, nil
		}
		return time.Time{}, true, fmt.Errorf("%s (as of %s ago)", reading.Error, roundedDuration(elapsed))
	}
	if reading.ServerAt.IsZero() {
		return time.Time{}, false, nil
	}
	window := agreeingReadingReuse
	if reading.CheckedAt.Sub(reading.ServerAt) > MaxClockSkew {
		window = holdingReadingReuse
	}
	if elapsed >= window {
		return time.Time{}, false, nil
	}
	return reading.ServerAt.Add(elapsed), true, nil
}

// clockProbePrefix is where ProbeServerClock writes its object: beside the
// setup connection test, the one place outside sessions/ agent-archive
// writes to, and only ever briefly.
const clockProbePrefix = ".setup-test/clock-"

// ProbeServerClock reads the storage service's current time: it writes a
// tiny object, reads the modification time the service stamped on it from a
// listing, and deletes it again. It returns an error rather than a time the
// service did not report.
func ProbeServerClock(ctx context.Context, store storage.ObjectStore) (time.Time, error) {
	id, err := local.ID()
	if err != nil {
		return time.Time{}, err
	}
	key := clockProbePrefix + id + ".json"
	if err := store.Put(ctx, key, []byte(`{"agent_archive_clock_check":true}`)); err != nil {
		return time.Time{}, fmt.Errorf("write clock check object: %w", err)
	}
	objects, listErr := store.List(ctx, key)
	if err := errors.Join(listErr, store.Delete(ctx, key)); err != nil {
		return time.Time{}, fmt.Errorf("read clock check object: %w", err)
	}
	for _, object := range objects {
		if object.Key == key && !object.LastModified.IsZero() {
			return object.LastModified, nil
		}
	}
	return time.Time{}, errors.New("storage reported no modification time for the clock check object")
}

// roundedDuration renders d for a message, to the minute (or the second,
// under one).
func roundedDuration(d time.Duration) time.Duration {
	if d < time.Minute {
		return d.Round(time.Second)
	}
	return d.Round(time.Minute)
}
