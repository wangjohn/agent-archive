package retention

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/storage"
)

const retentionWindow = 90 * 24 * time.Hour

// flakyStore fails every Put while down is set, standing in for a transient
// storage outage.
type flakyStore struct {
	storage.ObjectStore
	mu   sync.Mutex
	down bool
}

func (f *flakyStore) setDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

func (f *flakyStore) Put(ctx context.Context, key string, data []byte) error {
	f.mu.Lock()
	down := f.down
	f.mu.Unlock()
	if down {
		return errors.New("synthetic transient outage")
	}
	return f.ObjectStore.Put(ctx, key, data)
}

// recordingStore counts every remote call, so a test can assert a sweep never
// touched the bucket.
type recordingStore struct {
	storage.ObjectStore
	mu    sync.Mutex
	calls int
}

func (r *recordingStore) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *recordingStore) touch() {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
}

func (r *recordingStore) Get(ctx context.Context, key string) ([]byte, error) {
	r.touch()
	return r.ObjectStore.Get(ctx, key)
}

func (r *recordingStore) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	r.touch()
	return r.ObjectStore.List(ctx, prefix)
}

func (r *recordingStore) Delete(ctx context.Context, key string) error {
	r.touch()
	return r.ObjectStore.Delete(ctx, key)
}

func (r *recordingStore) Put(ctx context.Context, key string, data []byte) error {
	r.touch()
	return r.ObjectStore.Put(ctx, key, data)
}

func collect(t *testing.T, local *collector.LocalStore, store storage.ObjectStore, at time.Time) collector.Result {
	t.Helper()
	result, err := collector.Run(context.Background(), local, store, collector.Options{
		MachineID: "m", Now: func() time.Time { return at }, Retry: storage.RetryPolicy{MaxAttempts: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func sweep(t *testing.T, local *collector.LocalStore, store storage.ObjectStore, at time.Time, opts Options) Result {
	t.Helper()
	opts.Now = func() time.Time { return at }
	opts.SessionMaxAge = retentionWindow
	result, err := Sweep(context.Background(), local, store, opts)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func registered(t *testing.T, local *collector.LocalStore) int {
	t.Helper()
	regs, err := local.LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	return len(regs)
}

// A session resumed on the day it ages out, whose new publication then fails
// transiently, must not be expired on that same pass: expiry would delete the
// pending publication, the request, and its hook evidence before any of it was
// archived. The sweep waits, and expiry proceeds normally once the work lands.
func TestExpiryWaitsForPendingPublicationThenProceeds(t *testing.T) {
	dir := t.TempDir()
	local := newTestStore(t)
	store := &flakyStore{ObjectStore: storage.NewMemoryStore()}
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	path := writeTranscript(t, dir, "s1.jsonl", codexTranscript)
	if err := local.SaveRegistration(registration("s1", path)); err != nil {
		t.Fatal(err)
	}
	collect(t, local, store, t0)

	// The session is resumed 91 days later and its publication fails.
	resumed := t0.Add(91 * 24 * time.Hour)
	writeTranscript(t, dir, "s1.jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"m2","payload":{"type":"message","role":"user","content":"resumed"}}`)
	if err := local.SaveRequest("s1", "stop", resumed); err != nil {
		t.Fatal(err)
	}
	store.setDown(true)
	if result := collect(t, local, store, resumed); len(result.Errors) != 1 {
		t.Fatalf("publication was expected to fail: %#v", result)
	}
	if pending, err := local.HasPending("s1"); err != nil || !pending {
		t.Fatalf("no pending publication to protect: %t %v", pending, err)
	}

	result := sweep(t, local, store, resumed, Options{})
	if len(result.DeletedSessions) != 0 || len(result.Errors) != 0 {
		t.Fatalf("expired a session with unpublished work: %#v", result)
	}
	if registered(t, local) != 1 {
		t.Fatal("the registration was forgotten")
	}
	if pending, _ := local.HasPending("s1"); !pending {
		t.Fatal("the pending publication was deleted")
	}

	// Storage recovers: the new evidence is published, which also moves the
	// session's capture time, so it is no longer due.
	store.setDown(false)
	if result := collect(t, local, store, resumed.Add(time.Minute)); len(result.Published) != 1 {
		t.Fatalf("recovery did not publish: %#v", result)
	}
	if meta := fetchMetadata(t, store, "codex", "s1"); !meta.CapturedAt.Equal(resumed) {
		t.Fatalf("the resumed evidence was not the published snapshot: %s", meta.CapturedAt)
	}
	if result := sweep(t, local, store, resumed.Add(time.Hour), Options{}); len(result.DeletedSessions) != 0 {
		t.Fatalf("freshly published session expired: %#v", result)
	}
	// And it expires on its own schedule afterwards.
	if result := sweep(t, local, store, resumed.Add(91*24*time.Hour), Options{}); len(result.DeletedSessions) != 1 {
		t.Fatalf("session never expired after its work landed: %#v", result)
	}
}

func TestExpiryWaitsForPendingRequestThenProceeds(t *testing.T) {
	dir := t.TempDir()
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	path := writeTranscript(t, dir, "s1.jsonl", codexTranscript)
	if err := local.SaveRegistration(registration("s1", path)); err != nil {
		t.Fatal(err)
	}
	collect(t, local, store, t0)

	past := t0.Add(91 * 24 * time.Hour)
	if err := local.SaveRequest("s1", "stop", past); err != nil {
		t.Fatal(err)
	}
	if result := sweep(t, local, store, past, Options{}); len(result.DeletedSessions) != 0 || len(result.Errors) != 0 {
		t.Fatalf("expired a session with a pending request: %#v", result)
	}
	if requests, _ := local.LoadRequests(); len(requests) != 1 {
		t.Fatalf("the request was deleted: %#v", requests)
	}

	// The collector handles the request (nothing new: it is acknowledged),
	// and the next sweep expires the session.
	collect(t, local, store, past)
	if result := sweep(t, local, store, past, Options{}); len(result.DeletedSessions) != 1 {
		t.Fatalf("expiry did not proceed after the request cleared: %#v", result)
	}
}

// Deferral exists to protect work the collector will do. A session it no
// longer publishes (an excluded project, a deselected app) will never clear
// its request, so waiting on it would keep the session forever.
func TestUnpublishableSessionExpiresDespiteOutstandingWork(t *testing.T) {
	dir := t.TempDir()
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	path := writeTranscript(t, dir, "s1.jsonl", codexTranscript)
	if err := local.SaveRegistration(registration("s1", path)); err != nil {
		t.Fatal(err)
	}
	collect(t, local, store, t0)
	past := t0.Add(91 * 24 * time.Hour)
	if err := local.SaveRequest("s1", "stop", past); err != nil {
		t.Fatal(err)
	}
	result := sweep(t, local, store, past, Options{Publishable: func(archive.SessionRegistration) bool { return false }})
	if len(result.DeletedSessions) != 1 || registered(t, local) != 0 {
		t.Fatalf("an unpublishable session escaped expiry: %#v", result)
	}
	metaKey, _ := archive.MetadataObjectKey("codex", "s1")
	if _, err := store.Get(context.Background(), metaKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("its published metadata was left in the bucket: %v", err)
	}
}

// Sweep no longer skips anything: a registration is this machine's only
// record that the session's objects exist, whatever the collector thinks of
// publishing it now.
func TestSweepExpiresEveryRegistrationThisMachineOwns(t *testing.T) {
	dir := t.TempDir()
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	for _, id := range []string{"kept", "excluded"} {
		if err := local.SaveRegistration(registration(id, writeTranscript(t, dir, id+".jsonl", codexTranscript))); err != nil {
			t.Fatal(err)
		}
	}
	collect(t, local, store, t0)
	result := sweep(t, local, store, t0.Add(91*24*time.Hour), Options{
		Publishable: func(reg archive.SessionRegistration) bool { return reg.ArchiveSessionID != "excluded" },
	})
	if len(result.DeletedSessions) != 2 || registered(t, local) != 0 {
		t.Fatalf("result=%#v", result)
	}
}

// A session published to a previous destination has nothing in the current
// bucket. Once it ages out its local state is pruned, and the current bucket
// sees no request of any kind on its behalf.
func TestPreviousDestinationSessionIsPrunedLocallyWithoutTouchingTheBucket(t *testing.T) {
	dir := t.TempDir()
	local := newTestStore(t)
	previous := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := local.SaveRegistration(registration("old", writeTranscript(t, dir, "old.jsonl", codexTranscript))); err != nil {
		t.Fatal(err)
	}
	collect(t, local, previous, t0)
	if err := local.RecordSuperseded("old", "sessions/codex/old/source.stale.json.gz", t0); err != nil {
		t.Fatal(err)
	}

	current := &recordingStore{ObjectStore: storage.NewMemoryStore()}
	fromPrevious := func(archive.SessionRegistration) bool { return false }

	// Within the window: kept, and the bucket is not consulted either.
	if result := sweep(t, local, current, t0.Add(89*24*time.Hour), Options{CurrentDestination: fromPrevious}); len(result.PrunedSessions) != 0 || len(result.Errors) != 0 {
		t.Fatalf("pruned too early: %#v", result)
	}
	if registered(t, local) != 1 {
		t.Fatal("pruned too early")
	}

	result := sweep(t, local, current, t0.Add(91*24*time.Hour), Options{CurrentDestination: fromPrevious})
	if len(result.PrunedSessions) != 1 || result.PrunedSessions[0] != "old" || len(result.DeletedSessions) != 0 || len(result.Errors) != 0 {
		t.Fatalf("result=%#v", result)
	}
	if registered(t, local) != 0 {
		t.Fatal("local state for a previous destination was kept forever")
	}
	if ledger, _ := local.LoadSuperseded("old"); len(ledger) != 0 {
		t.Fatalf("superseded ledger survived pruning: %#v", ledger)
	}
	if calls := current.count(); calls != 0 {
		t.Fatalf("the current bucket was touched %d time(s) for a session it never held", calls)
	}
	// The previous bucket is not this machine's to clean any more.
	if meta := fetchMetadata(t, previous, "codex", "old"); meta.SessionID != "old" {
		t.Fatalf("previous destination was modified: %#v", meta)
	}
}
