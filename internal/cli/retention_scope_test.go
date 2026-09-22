package cli

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// cleanupCountingStore counts the calls only retention makes.
type cleanupCountingStore struct {
	storage.ObjectStore
	mu      sync.Mutex
	cleanup int
}

func (c *cleanupCountingStore) Delete(ctx context.Context, key string) error {
	c.mu.Lock()
	c.cleanup++
	c.mu.Unlock()
	return c.ObjectStore.Delete(ctx, key)
}

func (c *cleanupCountingStore) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	c.mu.Lock()
	c.cleanup++
	c.mu.Unlock()
	return c.ObjectStore.List(ctx, prefix)
}

func onlyRegistration(t *testing.T, home string) (archive.SessionRegistration, bool) {
	t.Helper()
	regs, err := collector.OpenLocalStoreReadOnly(home).LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) == 0 {
		return archive.SessionRegistration{}, false
	}
	return regs[0], true
}

// Excluding a project stops new publication. It must not also stop that
// project's already-published sessions from ageing out of the bucket, even
// with a hook request outstanding that the collector will never process.
func TestSyncStillExpiresSessionsOfAnExcludedProject(t *testing.T) {
	now := time.Now().UTC()
	env, home, _, bucket := publishedThroughSync(t, now)
	reg, _ := onlyRegistration(t, home)
	metadataKey, _ := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)

	cfg, _, _ := config.Load(home)
	cfg.Archive.Projects[0].Included = false
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.AcceptSession(reg) {
		t.Fatal("test precondition: the session should no longer be publishable")
	}
	if err := handleHookEvent(home, "codex", map[string]any{"hook_event_name": "Stop", "session_id": "native"}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	env.Now = func() time.Time { return now.Add(91 * 24 * time.Hour) }
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	if _, still := onlyRegistration(t, home); still {
		t.Fatal("an excluded project's session escaped retention")
	}
	if _, err := bucket.Get(context.Background(), metadataKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("its metadata was left in the bucket: %v", err)
	}
}

// A session from before the current destination was configured published to
// the previous bucket. Retention prunes its local state once it ages out and
// never sends a delete or listing to the current bucket for it.
func TestSyncPrunesPreviousDestinationSessionsWithoutTouchingTheBucket(t *testing.T) {
	now := time.Now().UTC()
	env, home, _, bucket := publishedThroughSync(t, now)
	cfg, _, _ := config.Load(home)
	cfg.DestinationSince = now.Add(time.Hour)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	counting := &cleanupCountingStore{ObjectStore: bucket}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return counting, nil }

	env.Now = func() time.Time { return now.Add(89 * 24 * time.Hour) }
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	if _, still := onlyRegistration(t, home); !still {
		t.Fatal("pruned before the retention window")
	}
	env.Now = func() time.Time { return now.Add(91 * 24 * time.Hour) }
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	if _, still := onlyRegistration(t, home); still {
		t.Fatal("local state for a previous destination was kept forever")
	}
	if counting.cleanup != 0 {
		t.Fatalf("retention sent %d delete/list call(s) to the current bucket for a session it never held", counting.cleanup)
	}
}
