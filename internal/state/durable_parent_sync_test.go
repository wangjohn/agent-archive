package state

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/wangjohn/agent-archive/internal/local"
)

func durableParentTestRoot(t *testing.T) *os.Root {
	t.Helper()
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	return root
}

func durableParentFDCount(t *testing.T) int {
	t.Helper()
	if runtime.GOOS != "linux" {
		return -1
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestDurableParentFirstNestedWriteOrdersBarriersBeforePayload(t *testing.T) {
	root := durableParentTestRoot(t)
	path := filepath.Join("sessions", "session", "pending-sources")
	parents := []string{root.Name(), filepath.Join(root.Name(), "sessions"), filepath.Join(root.Name(), "sessions", "session")}
	children := []string{"sessions", "session", "pending-sources"}
	events := []string{}
	dir, err := privateDirectoryWithParentSync(root, path, true, func(parent *os.Root) error {
		i := len(events)
		if i >= len(parents) {
			t.Fatal("unexpected additional barrier")
		}
		if info, err := parent.Lstat(children[i]); err != nil || !info.IsDir() {
			t.Fatalf("child was not created before its parent barrier: %v", err)
		}
		if _, err := root.Lstat(filepath.Join(path, "source.gz")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("payload allocated before ancestor barriers: %v", err)
		}
		events = append(events, filepath.Clean(parent.Name()))
		return syncDurableParent(parent)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dir.Close(); err != nil {
		t.Fatal(err)
	}
	if err := local.RootedAtomicWrite(root, filepath.Join(path, "source.gz"), func(w io.Writer) error {
		events = append(events, "payload")
		_, err := w.Write([]byte("frozen source"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := append(parents, "payload")
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("durable acknowledgement order: got %v want %v", events, want)
	}
}

func TestDurableParentBarrierFailureStopsAndClosesTraversal(t *testing.T) {
	for failAt := 0; failAt < 3; failAt++ {
		t.Run([]string{"home", "sessions", "session"}[failAt], func(t *testing.T) {
			root := durableParentTestRoot(t)
			before := durableParentFDCount(t)
			fault := errors.New("injected parent sync failure")
			var failedParent *os.Root
			calls := 0
			path := filepath.Join("sessions", "session", "pending-sources")
			dir, err := privateDirectoryWithParentSync(root, path, true, func(parent *os.Root) error {
				position := calls
				calls++
				if position == failAt {
					failedParent = parent
					return fault
				}
				return syncDurableParent(parent)
			})
			if dir != nil {
				_ = dir.Close()
			}
			if !errors.Is(err, fault) || dir != nil || calls != failAt+1 {
				t.Fatalf("barrier failure acknowledged or descended: dir=%v calls=%d err=%v", dir, calls, err)
			}
			if _, err := failedParent.Stat("."); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("failed containing root remained open: %v", err)
			}
			if after := durableParentFDCount(t); before >= 0 && after != before {
				t.Fatalf("traversal leaked parent or opened child descriptors: before=%d after=%d", before, after)
			}
			if failAt < 2 {
				unreached := []string{filepath.Join("sessions", "session"), path}[failAt]
				if _, err := root.Lstat(unreached); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed barrier allowed creation below its child: %v", err)
				}
			}
			if _, err := root.Lstat(filepath.Join(path, "source.gz")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed barrier acknowledged payload: %v", err)
			}
		})
	}
}

func TestDurableParentRetryExistingChildStillRequiresBarrier(t *testing.T) {
	root := durableParentTestRoot(t)
	if err := root.Mkdir("sessions", 0700); err != nil {
		t.Fatal(err)
	}
	if err := root.Mkdir(filepath.Join("sessions", "session"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("sessions", "session", "pending-sources")
	fault := errors.New("injected final parent sync failure")
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if info, err := root.Lstat(path); err != nil || !info.IsDir() {
				t.Fatalf("failed first barrier did not leave retry-existing child: %v", err)
			}
		}
		calls := 0
		dir, err := privateDirectoryWithParentSync(root, path, true, func(parent *os.Root) error {
			calls++
			if calls == 3 && attempt < 2 {
				return fault
			}
			return syncDurableParent(parent)
		})
		if attempt < 2 {
			if dir != nil {
				_ = dir.Close()
			}
			if !errors.Is(err, fault) || dir != nil || calls != 3 {
				t.Fatalf("attempt %d bypassed barrier: calls=%d dir=%v err=%v", attempt, calls, dir, err)
			}
			continue
		}
		if err != nil || dir == nil || calls != 3 {
			t.Fatalf("successful retry: calls=%d dir=%v err=%v", calls, dir, err)
		}
		if err := dir.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDurableParentExistingReadTraversalDoesNotSync(t *testing.T) {
	root := durableParentTestRoot(t)
	if err := root.Mkdir("sessions", 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := privateDirectoryWithParentSync(root, "sessions", false, func(*os.Root) error {
		t.Fatal("read-only traversal invoked a write barrier")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dir.Close(); err != nil {
		t.Fatal(err)
	}
}
