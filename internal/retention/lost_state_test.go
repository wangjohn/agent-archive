package retention

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func corruptFile(t *testing.T, local *state.Store, rel string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(local.Home(), rel), []byte(`{"trunc`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func sessionObjects(t *testing.T, store storage.ObjectStore, id string) int {
	t.Helper()
	objects, err := store.List(context.Background(), "sessions/codex/"+id+"/")
	if err != nil {
		t.Fatal(err)
	}
	return len(objects)
}

// A published state moved aside because it no longer decoded was the only
// local record that the session's objects are in the bucket. Once the
// transcript is gone too, the session looks never published; retention used
// to forget it locally without a bucket call, leaving its objects forever.
// It now deletes them like a published session's.
func TestSessionWhosePublishedStateWasLostIsStillDeletedFromTheBucket(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	dir := t.TempDir()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	publishTwice(t, local, store, "s1", dir, t0)
	if err := os.Remove(filepath.Join(dir, "s1.jsonl")); err != nil {
		t.Fatal(err)
	}
	corruptFile(t, local, "published/s1.json")
	if r := collect(t, local, store, t0.Add(time.Hour)); !errors.Is(r.Errors["s1"], state.ErrQuarantined) {
		t.Fatalf("errors = %v", r.Errors)
	}
	collect(t, local, store, t0.Add(2*time.Hour))
	if summary, found, _ := local.LoadPublishedSummary("s1"); found && summary.Published {
		t.Fatal("test precondition: the session should look never published")
	}
	result := sweep(t, local, store, t0.Add(retentionWindow+time.Hour), Options{})
	if len(result.Errors) != 0 || len(result.DeletedSessions) != 1 {
		t.Fatalf("%#v", result)
	}
	if n := sessionObjects(t, store, "s1"); n != 0 {
		t.Fatalf("%d object(s) of a session that lost its published state outlived retention", n)
	}
	if quarantined := local.QuarantinedFiles(); len(quarantined) != 0 {
		t.Fatalf("the moved-aside copy outlived its session: %v", quarantined)
	}
}

// A registration moved aside drops its session out of the registered set, so
// retention never swept it and its objects outlived retention in the bucket
// and on this machine. The session's remaining state marks it as an orphan,
// which is aged from its capture and deleted the same way.
func TestSessionWhoseRegistrationWasLostIsStillExpired(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	dir := t.TempDir()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	publishTwice(t, local, store, "s1", dir, t0)
	publishTwice(t, local, store, "s2", dir, t0)
	corruptFile(t, local, "registrations/s1.json")
	if r := collect(t, local, store, t0.Add(time.Hour)); !errors.Is(r.Errors["s1"], state.ErrQuarantined) {
		t.Fatalf("errors = %v", r.Errors)
	}
	// Within the window: nothing happens to either.
	if result := sweep(t, local, store, t0.Add(retentionWindow-time.Hour), Options{}); len(result.DeletedSessions) != 0 || len(result.Errors) != 0 {
		t.Fatalf("%#v", result)
	}
	if sessionObjects(t, store, "s1") == 0 {
		t.Fatal("an orphan was deleted before its window")
	}
	result := sweep(t, local, store, t0.Add(retentionWindow+time.Hour), Options{})
	if len(result.Errors) != 0 || len(result.DeletedSessions) != 2 {
		t.Fatalf("%#v", result)
	}
	if n := sessionObjects(t, store, "s1"); n != 0 {
		t.Fatalf("%d object(s) of the orphan outlived retention", n)
	}
	for _, dir := range []string{"published", "superseded"} {
		if _, err := os.Stat(filepath.Join(local.Home(), dir, "s1.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the orphan's %s state outlived retention: %v", dir, err)
		}
	}
}

// An orphan is expired by age like any other session, so a clock ahead of
// the storage service's deletes nothing of it either.
func TestOrphanIsKeptWhileTheClockIsAhead(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	publishTwice(t, local, store, "s1", t.TempDir(), t0)
	corruptFile(t, local, "registrations/s1.json")
	if r := collect(t, local, store, t0.Add(time.Hour)); !errors.Is(r.Errors["s1"], state.ErrQuarantined) {
		t.Fatalf("errors = %v", r.Errors)
	}
	at := t0.Add(retentionWindow + time.Hour)
	ahead := func(context.Context) (time.Time, error) { return at.Add(-2 * MaxClockSkew), nil }
	result, err := Sweep(context.Background(), local, store, Options{Now: func() time.Time { return at }, ServerClock: ahead, SessionMaxAge: retentionWindow})
	if err != nil || len(result.DeletedSessions) != 0 || !errors.Is(result.Held, ErrClockAhead) {
		t.Fatalf("%#v %v", result, err)
	}
	if sessionObjects(t, store, "s1") == 0 {
		t.Fatal("a clock ahead deleted an orphan's objects")
	}
	if orphans, err := local.OrphanedSessions(nil); err != nil || len(orphans) != 1 {
		t.Fatalf("a clock ahead forgot an orphan: %v %v", orphans, err)
	}
}

// An orphan is not forgotten if its registration reappears before the sweep
// gets to it: a hook registering the native session again reuses its ID.
func TestOrphanRegisteredAgainIsKept(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	publishTwice(t, local, store, "s1", t.TempDir(), t0)
	if forgotten, err := local.ForgetOrphan("s1"); err != nil || forgotten {
		t.Fatalf("forgot a registered session: %v %v", forgotten, err)
	}
	if orphans, err := local.OrphanedSessions(nil); err != nil || len(orphans) != 0 {
		t.Fatalf("orphans = %v %v", orphans, err)
	}
}

// A sweep with nothing to delete reads each session's summary, not its
// source bundles: it runs after every collector pass, over every session.
func TestSweepWithNothingToDeleteDecodesNoPublishedState(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	dir := t.TempDir()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	for _, id := range []string{"s1", "s2", "s3"} {
		publishTwice(t, local, store, id, dir, t0)
	}
	loads := state.PublishedStateLoads()
	result := sweep(t, local, store, t0.Add(time.Hour), Options{})
	if len(result.Errors) != 0 || result.DeletedSnapshots != 0 {
		t.Fatalf("%#v", result)
	}
	if n := state.PublishedStateLoads() - loads; n != 0 {
		t.Fatalf("a sweep with nothing to delete decoded %d published states", n)
	}
}
