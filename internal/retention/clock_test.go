package retention

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/storage"
)

// agreeing gives opts a storage clock that reads exactly what its Now does,
// the way a test moves time forward on both at once. Without it the storage
// clock is the probe of a MemoryStore, which reads the real time.
func agreeing(opts Options) Options {
	if opts.ServerClock == nil {
		opts.ServerClock = func(context.Context) (time.Time, error) { return opts.now(), nil }
	}
	return opts
}

// probeCountingStore counts the clock probes a sweep writes.
type probeCountingStore struct {
	*storage.MemoryStore
	mu      sync.Mutex
	probes  int
	failPut bool
}

func (s *probeCountingStore) Put(ctx context.Context, key string, data []byte) error {
	if strings.HasPrefix(key, clockProbePrefix) {
		s.mu.Lock()
		s.probes++
		s.mu.Unlock()
		if s.failPut {
			return errors.New("synthetic outage")
		}
	}
	return s.MemoryStore.Put(ctx, key, data)
}

func objectCount(t *testing.T, store storage.ObjectStore) int {
	t.Helper()
	objects, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return len(objects)
}

// A clock set a year ahead (a dead RTC, a bad NTP answer, a VM restore, a
// user trying a trial) used to delete every session in the bucket within one
// pass: every age retention compares was stamped by that same clock, and the
// storage SDK corrects for skew rather than refusing. The storage service's
// own clock now has to agree first, so nothing is deleted, remotely or
// locally, and the sweep says why.
func TestClockAheadOfStorageDeletesNothing(t *testing.T) {
	local := newTestStore(t)
	store := &probeCountingStore{MemoryStore: storage.NewMemoryStore()}
	dir := t.TempDir()
	now := time.Now().UTC()
	for _, id := range []string{"s1", "s2", "s3"} {
		publishTwice(t, local, store, id, dir, now)
	}
	// Never published: only local state, which a jumped clock must not
	// forget either.
	if err := local.SaveRegistration(registration("bare", filepath.Join(dir, "missing.jsonl"))); err != nil {
		t.Fatal(err)
	}
	before := objectCount(t, store)

	wrong := now.Add(365 * 24 * time.Hour)
	result, err := Sweep(context.Background(), local, store, Options{Now: func() time.Time { return wrong }, SessionMaxAge: 90 * 24 * time.Hour})
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	if len(result.DeletedSessions)+len(result.PrunedSessions) != 0 || result.DeletedSnapshots != 0 {
		t.Fatalf("a clock a year ahead deleted evidence: %#v", result)
	}
	if !errors.Is(result.Held, ErrClockAhead) {
		t.Fatalf("held = %v, want ErrClockAhead", result.Held)
	}
	if after := objectCount(t, store); after != before {
		t.Fatalf("objects before=%d after=%d", before, after)
	}
	if registered(t, local) != 4 {
		t.Fatal("registrations were forgotten")
	}
	// One probe for the whole sweep, however many sessions wanted deleting.
	if store.probes != 1 {
		t.Fatalf("probes = %d, want 1", store.probes)
	}
}

// The same sweep, once the storage service agrees, expires everything: the
// check holds deletion only while the clocks disagree.
func TestClockThatAgreesWithStorageExpiresAsBefore(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	t0 := time.Now().UTC().Add(-100 * 24 * time.Hour)
	publishTwice(t, local, store, "s1", t.TempDir(), t0)
	// Now is the real time here, as is the MemoryStore's clock: the default
	// probe runs and agrees.
	result, err := Sweep(context.Background(), local, store, Options{SessionMaxAge: 90 * 24 * time.Hour})
	if err != nil || len(result.Errors) != 0 || result.Held != nil || len(result.DeletedSessions) != 1 {
		t.Fatalf("%#v %v", result, err)
	}
	if n := objectCount(t, store); n != 0 {
		t.Fatalf("%d object(s) left, the clock probe's included", n)
	}
}

// A clock behind the storage service's only delays deletion, the safe
// direction, so it is let through.
func TestClockBehindStorageStillDeletes(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	publishTwice(t, local, store, "s1", t.TempDir(), t0)
	at := t0.Add(100 * 24 * time.Hour)
	behind := func(context.Context) (time.Time, error) { return at.Add(30 * 24 * time.Hour), nil }
	result, err := Sweep(context.Background(), local, store, Options{Now: func() time.Time { return at }, ServerClock: behind, SessionMaxAge: 90 * 24 * time.Hour})
	if err != nil || result.Held != nil || len(result.DeletedSessions) != 1 {
		t.Fatalf("%#v %v", result, err)
	}
}

// When the storage service's clock cannot be read, nothing is deleted on
// account of age: the check fails closed.
func TestUnreadableStorageClockHoldsDeletion(t *testing.T) {
	local := newTestStore(t)
	store := &probeCountingStore{MemoryStore: storage.NewMemoryStore()}
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	publishTwice(t, local, store, "s1", t.TempDir(), t0)
	store.failPut = true
	at := t0.Add(100 * 24 * time.Hour)
	result, err := Sweep(context.Background(), local, store, Options{Now: func() time.Time { return at }, SessionMaxAge: 90 * 24 * time.Hour})
	if err != nil || len(result.DeletedSessions) != 0 || result.DeletedSnapshots != 0 || !errors.Is(result.Held, ErrClockUnverified) {
		t.Fatalf("%#v %v", result, err)
	}
}

// A Now far past the previous collector pass waits one pass before deleting
// anything, whatever the storage service says; with a recent previous pass
// the same sweep proceeds.
func TestClockFarPastThePreviousPassWaitsOnePass(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	publishTwice(t, local, store, "s1", t.TempDir(), t0)
	at := t0.Add(100 * 24 * time.Hour)
	opts := agreeing(Options{Now: func() time.Time { return at }, SessionMaxAge: 90 * 24 * time.Hour, PreviousScanAt: at.Add(-2 * MaxPassGap)})
	result, err := Sweep(context.Background(), local, store, opts)
	if err != nil || len(result.DeletedSessions) != 0 || !errors.Is(result.Held, ErrClockJumped) {
		t.Fatalf("%#v %v", result, err)
	}
	opts.PreviousScanAt = at.Add(-time.Minute)
	result, err = Sweep(context.Background(), local, store, opts)
	if err != nil || result.Held != nil || len(result.DeletedSessions) != 1 {
		t.Fatalf("%#v %v", result, err)
	}
}

// Superseded snapshots are deleted by age too, so the same check guards them.
func TestClockAheadKeepsSupersededSnapshots(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	now := time.Now().UTC()
	dir := t.TempDir()
	first := publishTwice(t, local, store, "s1", dir, now)
	publishThird(t, local, store, "s1", dir, now.Add(time.Hour))
	result, err := Sweep(context.Background(), local, store, Options{Now: func() time.Time { return now.Add(30 * 24 * time.Hour) }})
	if err != nil || result.DeletedSnapshots != 0 || !errors.Is(result.Held, ErrClockAhead) {
		t.Fatalf("%#v %v", result, err)
	}
	if _, err := store.Get(context.Background(), first); err != nil {
		t.Fatalf("superseded snapshot deleted by a clock that was ahead: %v", err)
	}
}

// The mirror case: evidence captured while the clock was ahead carries a
// capture time in the future. Once the clock is right again it used to keep
// the session past retention until that date came round. A capture time in
// the future is clamped to now, durably, once storage confirms the clock: the
// session then expires a window after the clamp, not a window after a date
// that has not happened.
func TestCaptureStampedByAClockAheadIsClampedOnceTheClockIsRight(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	right := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	// Captured while the clock read sixty days ahead.
	publishTwice(t, local, store, "s1", t.TempDir(), right.Add(60*24*time.Hour))

	result, err := Sweep(context.Background(), local, store, agreeing(Options{Now: func() time.Time { return right }, SessionMaxAge: 90 * 24 * time.Hour}))
	if err != nil || len(result.Errors) != 0 || len(result.DeletedSessions) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	summary, _, err := local.LoadPublishedSummary("s1")
	if err != nil || !summary.AgeFrom.Equal(right) {
		t.Fatalf("clamp = %v (%v), want %v", summary.AgeFrom, err, right)
	}
	// A window after the clamp, and only thirty days after the stamped time
	// had it been kept: expired.
	later := right.Add(91 * 24 * time.Hour)
	result, err = Sweep(context.Background(), local, store, agreeing(Options{Now: func() time.Time { return later }, SessionMaxAge: 90 * 24 * time.Hour}))
	if err != nil || len(result.DeletedSessions) != 1 {
		t.Fatalf("%#v %v", result, err)
	}
}

// A future capture time is not clamped while the clock is unconfirmed: a
// clock set far behind would otherwise make every correct capture time look
// like the future, clamp it to that past date, and expire it all once the
// clock was corrected.
func TestFutureCaptureIsNotClampedByAClockBehindStorage(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	captured := time.Date(2026, 6, 1, 1, 0, 0, 0, time.UTC)
	publishTwice(t, local, store, "s1", t.TempDir(), captured)
	behind := captured.Add(-365 * 24 * time.Hour)
	serverNow := func(context.Context) (time.Time, error) { return captured, nil }
	result, err := Sweep(context.Background(), local, store, Options{Now: func() time.Time { return behind }, ServerClock: serverNow, SessionMaxAge: 90 * 24 * time.Hour})
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	if summary, _, _ := local.LoadPublishedSummary("s1"); !summary.AgeFrom.IsZero() {
		t.Fatalf("clamped to %v by a clock a year behind", summary.AgeFrom)
	}
}

// The ledger's supersession times get the same clamp.
func TestSupersessionStampedByAClockAheadIsClamped(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	right := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	ahead := right.Add(60 * 24 * time.Hour)
	first := publishTwice(t, local, store, "s1", dir, ahead)
	publishThird(t, local, store, "s1", dir, ahead.Add(time.Hour))
	sweepAt := func(at time.Time) Result {
		t.Helper()
		result, err := Sweep(context.Background(), local, store, agreeing(Options{Now: func() time.Time { return at }}))
		if err != nil || len(result.Errors) != 0 {
			t.Fatalf("%#v %v", result, err)
		}
		return result
	}
	sweepAt(right)
	ledger, err := local.LoadSuperseded("s1")
	if err != nil || len(ledger) != 2 || !ledger[0].SupersededAt.Equal(right) || !ledger[1].SupersededAt.Equal(right) {
		t.Fatalf("ledger = %#v (%v)", ledger, err)
	}
	if result := sweepAt(right.Add(25 * time.Hour)); result.DeletedSnapshots != 1 {
		t.Fatalf("%#v", result)
	}
	if _, err := store.Get(context.Background(), first); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("the oldest snapshot outlived its grace period: %v", err)
	}
}

// The probe leaves nothing in the bucket and reads the service's time.
func TestProbeServerClockLeavesNothingBehind(t *testing.T) {
	store := storage.NewMemoryStore()
	before := time.Now().Add(-time.Second)
	at, err := ProbeServerClock(context.Background(), store)
	if err != nil || at.Before(before) || at.After(time.Now().Add(time.Second)) {
		t.Fatalf("at=%v err=%v", at, err)
	}
	if n := objectCount(t, store); n != 0 {
		t.Fatalf("%d object(s) left behind", n)
	}
}

// A sweep past its deadline stops cleanly, as a collector pass does: the
// sessions it did not reach are left for the next sweep, not reported as
// failing (which made sync exit 1 and status say "Needs attention" whenever
// a large import expired at once).
func TestSweepPastItsDeadlineLeavesTheRestForTheNextSweep(t *testing.T) {
	local := newTestStore(t)
	mem := storage.NewMemoryStore()
	dir := t.TempDir()
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	const sessions = 30
	for i := range sessions {
		publishTwice(t, local, mem, fmt.Sprintf("s%02d", i), dir, t0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	opts := agreeing(Options{Now: func() time.Time { return t0.Add(100 * 24 * time.Hour) }, SessionMaxAge: 90 * 24 * time.Hour})
	result, err := Sweep(ctx, local, slowStore{mem, 10 * time.Millisecond}, opts)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("errors = %v, err = %v", result.Errors, err)
	}
	if result.Unfinished == 0 || len(result.DeletedSessions)+result.Unfinished != sessions {
		t.Fatalf("deleted=%d unfinished=%d", len(result.DeletedSessions), result.Unfinished)
	}
	result, err = Sweep(context.Background(), local, mem, opts)
	if err != nil || len(result.Errors) != 0 || registered(t, local) != 0 {
		t.Fatalf("the next sweep did not finish: %#v %v", result, err)
	}
}

// slowStore delays every call, and gives up when its context ends.
type slowStore struct {
	*storage.MemoryStore
	d time.Duration
}

func (s slowStore) wait(ctx context.Context) error {
	select {
	case <-time.After(s.d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s slowStore) Get(ctx context.Context, k string) ([]byte, error) {
	if err := s.wait(ctx); err != nil {
		return nil, err
	}
	return s.MemoryStore.Get(ctx, k)
}

func (s slowStore) Delete(ctx context.Context, k string) error {
	if err := s.wait(ctx); err != nil {
		return err
	}
	return s.MemoryStore.Delete(ctx, k)
}

func (s slowStore) List(ctx context.Context, p string) ([]storage.Object, error) {
	if err := s.wait(ctx); err != nil {
		return nil, err
	}
	return s.MemoryStore.List(ctx, p)
}
