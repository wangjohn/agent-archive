package state

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

// Every entry a Store owns has a decided answer to "what if this file no
// longer decodes?". A directory added without one fails here, before it can
// become a file that fails its session on every pass with no way out.
func TestEveryOwnedEntryHasACorruptionPolicy(t *testing.T) {
	owned := OwnedEntries()
	for _, entry := range owned {
		if _, ok := corruptionPolicies[entry]; !ok {
			t.Errorf("%s has no corruption policy", entry)
		}
	}
	for entry := range corruptionPolicies {
		if !slices.Contains(owned, entry) {
			t.Errorf("corruption policy for %s, which the store does not own", entry)
		}
	}
}

func writeCorrupt(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"archive_se`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Only a collector pass, the files' only writer, moves a collector-owned file
// aside; any other reader reports it and leaves it.
func TestCollectorOwnedFilesAreMovedAsideOnlyInAPass(t *testing.T) {
	store := newTestStore(t)
	writeCorrupt(t, store.pendingPath("session-1"))
	if _, _, err := store.LoadPending("session-1"); err == nil || errors.Is(err, ErrQuarantined) {
		t.Fatalf("outside a pass: %v", err)
	}
	if _, err := os.Stat(store.pendingPath("session-1")); err != nil {
		t.Fatal("moved aside outside a pass")
	}
	pass := store.ForCollectorPass()
	if _, _, err := pass.LoadPending("session-1"); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("in a pass: %v", err)
	}
	if _, found, err := pass.LoadPending("session-1"); found || err != nil {
		t.Fatalf("after: %v %v", found, err)
	}
	if !store.LostPublication("session-1") {
		t.Fatal("the lost pending publication is not remembered")
	}
}

// A pending publication holds bytes not yet uploaded. Only one whose bytes
// are no longer JSON at all is moved aside; one that is JSON of another shape
// (written by a newer version, then downgraded) is reported and left where
// it is, so the version that wrote it can still upload it.
func TestPendingOfAnotherShapeIsNeverMovedAside(t *testing.T) {
	pass := newTestStore(t).ForCollectorPass()
	path := pass.pendingPath("session-1")
	if err := os.WriteFile(path, []byte(`{"source_key":7,"source_bytes":"AAAA"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pass.LoadPending("session-1"); err == nil || errors.Is(err, ErrQuarantined) {
		t.Fatalf("err = %v, want a reported error that moved nothing", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a pending publication of another shape was moved: %v", err)
	}
	if pass.LostPublication("session-1") {
		t.Fatal("reported as lost")
	}
}

// The native-session index is derived from the registrations: an entry that
// no longer decodes is recovered from the registration naming the native
// session, so the session keeps its archive ID instead of getting a second.
func TestCorruptSessionIndexIsRecoveredFromTheRegistration(t *testing.T) {
	store := newTestStore(t)
	reg, err := store.RegisterNewSession("native-1", func(id string) (r archive.SessionRegistration) {
		r = registration(t)
		r.ArchiveSessionID = id
		return r
	})
	if err != nil {
		t.Fatal(err)
	}
	writeCorrupt(t, nativeSessionIndexPath(store.home, "native-1"))
	id, found, err := store.ArchiveSessionID("native-1")
	if err != nil || !found || id != reg.ArchiveSessionID {
		t.Fatalf("recovered %q %v %v, want %q", id, found, err, reg.ArchiveSessionID)
	}
	id, created, err := store.EnsureArchiveSessionID("native-1")
	if err != nil || created || id != reg.ArchiveSessionID {
		t.Fatalf("ensure = %q %v %v", id, created, err)
	}
	var entry sessionIndexEntry
	if err := local.Read(nativeSessionIndexPath(store.home, "native-1"), &entry); err != nil || entry.ArchiveSessionID != reg.ArchiveSessionID {
		t.Fatalf("index not repaired: %#v %v", entry, err)
	}
	// With no registration to recover from, a fresh ID replaces the entry.
	writeCorrupt(t, nativeSessionIndexPath(store.home, "native-2"))
	if id, created, err := store.EnsureArchiveSessionID("native-2"); err != nil || !created || id == "" {
		t.Fatalf("fresh = %q %v %v", id, created, err)
	}
}

// A removal record that no longer decodes still says the session was
// removed, so backfill keeps skipping it.
func TestCorruptRemovalRecordStillCountsAsRemoved(t *testing.T) {
	store := newTestStore(t)
	if err := store.RecordRemoval("codex", "native-1", RemovalReasonRetention, time.Now()); err != nil {
		t.Fatal(err)
	}
	writeCorrupt(t, removalPath(store.home, "codex", "native-1"))
	if _, found, err := store.Removal("codex", "native-1"); err != nil || !found {
		t.Fatalf("%v %v", found, err)
	}
}
