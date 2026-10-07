//go:build darwin || linux

package state

import (
	"errors"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

func TestTemporaryQuotaConcurrentFirstUseSerializesIndependentRoots(t *testing.T) {
	t.Parallel()
	// Each iteration starts without a lock file, as two first-use consumers
	// would after restart. Independent roots exercise the actual openat race.
	for range 64 {
		home := t.TempDir()
		var roots [2]*os.Root
		for i := range roots {
			root, err := os.OpenRoot(home)
			if err != nil {
				t.Fatal(err)
			}
			roots[i] = root
		}
		start := make(chan struct{})
		var workers sync.WaitGroup
		var active atomic.Int32
		var overlap atomic.Bool
		var failures [2]error
		for i, root := range roots {
			workers.Go(func() {
				<-start
				unlock, err := temporaryQuotaLock(root, time.Second)
				failures[i] = err
				if err != nil {
					return
				}
				defer unlock()
				if active.Add(1) != 1 {
					overlap.Store(true)
				}
				runtime.Gosched()
				active.Add(-1)
			})
		}
		close(start)
		workers.Wait()
		for _, root := range roots {
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if failures[0] != nil || failures[1] != nil || overlap.Load() {
			t.Fatalf("first-use quota lock: errors=%v overlap=%v", failures, overlap.Load())
		}
	}
}

func TestTemporaryQuotaLockContentionKeepsOriginalDeadline(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	first, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	unlock, err := temporaryQuotaLock(first, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	started := time.Now()
	if release, err := temporaryQuotaLock(second, 40*time.Millisecond); !errors.Is(err, local.ErrBusy) {
		if release != nil {
			release()
		}
		t.Fatal("contended lock did not expire", err)
	}
	if elapsed := time.Since(started); elapsed < 40*time.Millisecond || elapsed > time.Second {
		t.Fatal("lock deadline changed", elapsed)
	}
}

func TestTemporaryQuotaLockOpenErrorsStayBounded(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	root, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err = root.Mkdir("temporary-quota", 0700); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if release, err := temporaryQuotaLock(root, time.Second); err == nil || errors.Is(err, os.ErrNotExist) {
		if release != nil {
			release()
		}
		t.Fatal("nontransient open error lost", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatal("nontransient open error retried", elapsed)
	}
	if err = os.RemoveAll(home); err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	if release, err := temporaryQuotaLock(root, 40*time.Millisecond); !errors.Is(err, os.ErrNotExist) {
		if release != nil {
			release()
		}
		t.Fatal("persistent absence lost", err)
	}
	if elapsed := time.Since(started); elapsed < 40*time.Millisecond || elapsed > time.Second {
		t.Fatal("absence retries extended deadline", elapsed)
	}
}
