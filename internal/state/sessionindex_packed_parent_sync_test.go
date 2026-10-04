package state

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

func TestPackedParentSyncGroupsCommittedMembersAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name           string
		mixed          bool
		secondRejected bool
		fail           bool
		want           int
	}{
		{name: "shared", want: 1}, {name: "mixed", mixed: true, want: 2},
		{name: "shared failure", fail: true, want: 1}, {name: "second rejected", secondRejected: true, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			batch := []packedPublication{{path: filepath.Join(home, "a", "00.idx")}, {path: filepath.Join(home, "a", "01.idx")}}
			if tc.mixed {
				batch[1].path = filepath.Join(home, "b", "01.idx")
			}
			staged := make([]*local.Staged, 2)
			errs := make([]error, 2)
			failure := errors.New("native directory sync failed")
			rejected := errors.New("second guard rejected")
			for i := range batch {
				if i == 1 && tc.secondRejected {
					errs[i] = rejected
					continue
				}
				var err error
				staged[i], err = local.StageBytes(batch[i].path, []byte("durable"))
				if err != nil {
					t.Fatal(err)
				}
				defer staged[i].Discard()
				if err := staged[i].Commit(); err != nil {
					t.Fatal(err)
				}
			}
			var calls atomic.Int32
			syncPackedParents(batch, staged, errs, func(s *local.Staged) error {
				calls.Add(1)
				// Every committed rename must already be visible before shared durability.
				for i := range batch {
					if staged[i] != nil {
						if data, err := os.ReadFile(batch[i].path); err != nil || string(data) != "durable" {
							t.Errorf("rename missing: %q %v", data, err)
						}
					}
				}
				if tc.fail {
					return failure
				}
				return s.SyncDir()
			})
			if int(calls.Load()) != tc.want {
				t.Fatalf("sync calls %d want %d", calls.Load(), tc.want)
			}
			for i := range errs {
				if staged[i] != nil && errors.Is(errs[i], failure) != tc.fail {
					t.Fatalf("shared failure attribution %v", errs)
				}
			}
			if tc.secondRejected && !errors.Is(errs[1], rejected) {
				t.Fatalf("guard error lost: %v", errs)
			}
		})
	}
}

func TestPackedParentSyncJoinsEveryParentAfterFailure(t *testing.T) {
	batch := []packedPublication{{path: "a/00.idx"}, {path: "b/01.idx"}}
	// The injected sync does not inspect these tokens; production passes real
	// committed staged files and the native SyncDir method.
	staged := []*local.Staged{{}, {}}
	errs := make([]error, 2)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	done := make(chan struct{})
	failure := errors.New("directory failed")
	go func() {
		syncPackedParents(batch, staged, errs, func(s *local.Staged) error {
			entered <- struct{}{}
			if s == staged[0] {
				return failure
			}
			<-release
			return nil
		})
		close(done)
	}()
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("parent sync not started")
		}
	}
	select {
	case <-done:
		t.Fatal("returned with native sync pending")
	default:
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("native sync not joined")
	}
	if !errors.Is(errs[0], failure) || errs[1] != nil {
		t.Fatalf("independent errors: %v", errs)
	}
}
