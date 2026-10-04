package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/testutil/recoverytest"
)

func TestCompletedCensusRepairsRecreatedEmptyIndexesOnce(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	reg := registration(t)
	if err := store.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err := recoverytest.Exhaust(context.Background(), store, SessionIndexRecoverySlice, false); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sessions", "sessions-v1"} {
		if err := os.RemoveAll(filepath.Join(store.Home(), name)); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	store, err = Open(store.Home())
	if err != nil {
		t.Fatal(err)
	}
	censuses := 0
	store.onIndexStep = func(step string) error {
		if step == "recovery-begin" {
			censuses++
		}
		return nil
	}
	for range 3 {
		if err := recoverytest.Exhaust(context.Background(), store, SessionIndexRecoverySlice, false); err != nil {
			t.Fatal(err)
		}
	}
	if censuses != 1 {
		t.Fatalf("lost indexes required %d censuses, want one", censuses)
	}
	key, err := agentmeta.NewSessionKey(reg.Harness.Name, reg.NativeSessionID)
	if err != nil {
		t.Fatal(err)
	}
	id, found, err := store.ArchiveSessionID(key)
	if err != nil || !found || id != reg.ArchiveSessionID {
		t.Fatalf("authority not restored: id=%q found=%v err=%v", id, found, err)
	}
}

func TestCompletedEmptyCensusDoesNotRepeat(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	if err := recoverytest.Exhaust(context.Background(), store, SessionIndexRecoverySlice, false); err != nil {
		t.Fatal(err)
	}
	store.onIndexStep = func(string) error {
		t.Fatal("healthy empty store repeated recovery")
		return nil
	}
	for range 3 {
		if err := recoverytest.Exhaust(context.Background(), store, SessionIndexRecoverySlice, false); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDirectoryPresenceProbeDoesNotInspectEntries(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, path := range []string{root, filepath.Join(root, "missing")} {
		present, err := directoryHasEntry(path)
		if err != nil || present {
			t.Fatalf("empty directory probe: present=%v err=%v", present, err)
		}
	}
	// A broken entry is enough to distinguish a nonempty directory. No entry
	// stat, content read, or full inventory is needed for this availability hint.
	if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "entry")); err != nil {
		t.Fatal(err)
	}
	if present, err := directoryHasEntry(root); err != nil || !present {
		t.Fatalf("nonempty directory probe: present=%v err=%v", present, err)
	}
	file := filepath.Join(t.TempDir(), "not-directory")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if present, err := directoryHasEntry(file); err == nil || present {
		t.Fatalf("invalid directory accepted: present=%v err=%v", present, err)
	}
}
