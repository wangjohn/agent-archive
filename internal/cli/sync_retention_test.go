package cli

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
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
	regs, err := state.OpenReadOnly(home).LoadRegistrations()
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
	t.Parallel()
	now := time.Now().UTC()
	env, home, _, bucket := publishedThroughSync(t, now)
	storageClockFollows(t, &env, func() time.Time { return env.Now() })
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
	if err := capture.HandleEvent(home, "codex", map[string]any{"hook_event_name": "Stop", "session_id": "native"}, now.Add(time.Hour)); err != nil {
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

// A session admitted into a previous destination published to that bucket.
// Retention prunes its local state once it ages out and never sends a delete
// or listing to the current bucket for it. A registration with a destination
// ID is judged by it: after a switch away and back, its objects are in the
// current bucket again and retention deletes them there. One without an ID
// (written before the ID existed) is judged by time, as before.
func TestSyncPrunesPreviousDestinationSessionsWithoutTouchingTheBucket(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		change     func(cfg *config.Config, now time.Time)
		legacy     bool
		wantBucket bool
	}{
		{name: "destination changed", change: func(cfg *config.Config, now time.Time) {
			cfg.PreviousDestinations = append(cfg.PreviousDestinations, cfg.Storage)
			cfg.Storage.Bucket = "new-bucket"
			cfg.DestinationSince = now.Add(time.Hour)
		}},
		{name: "legacy registration, switched back", legacy: true, change: func(cfg *config.Config, now time.Time) {
			cfg.DestinationSince = now.Add(time.Hour)
		}},
		{name: "switched back", wantBucket: true, change: func(cfg *config.Config, now time.Time) {
			cfg.DestinationSince = now.Add(time.Hour)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := time.Now().UTC()
			env, home, _, bucket := publishedThroughSync(t, now)
			storageClockFollows(t, &env, func() time.Time { return env.Now() })
			reg, _ := onlyRegistration(t, home)
			if reg.DestinationID == "" {
				t.Fatal("the hook registered the session without its destination ID")
			}
			if tc.legacy {
				reg.DestinationID = ""
				store, err := state.Open(home)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.SaveRegistration(reg); err != nil {
					t.Fatal(err)
				}
			}
			metadataKey, _ := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
			cfg, _, _ := config.Load(home)
			tc.change(&cfg, now)
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
				t.Fatal("local state was kept past retention")
			}
			_, getErr := bucket.Get(context.Background(), metadataKey)
			if tc.wantBucket {
				if counting.cleanup == 0 || !errors.Is(getErr, storage.ErrNotFound) {
					t.Fatalf("the session's objects were left in the bucket it was admitted into: %d call(s), %v", counting.cleanup, getErr)
				}
				return
			}
			if counting.cleanup != 0 {
				t.Fatalf("retention sent %d delete/list call(s) to the current bucket for a session it never held", counting.cleanup)
			}
		})
	}
}
