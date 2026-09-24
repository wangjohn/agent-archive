package state

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

func registrationFor(id, native string) archive.SessionRegistration {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return archive.SessionRegistration{
		ArchiveSessionID: id, NativeSessionID: native, ProjectID: "project-1", ProjectRoot: "/p",
		Harness: archive.Harness{Name: "claude"}, TranscriptPath: "/t.jsonl", SessionStartedAt: at, RegisteredAt: at,
	}
}

// A registration forgotten before the update gets the lock is reported, not
// written back; an update error saves nothing.
func TestUpdateRegistrationReportsAForgottenSessionAndSavesNothingOnError(t *testing.T) {
	local := newTestStore(t)
	reg, err := local.RegisterNewSession("native-1", func(id string) archive.SessionRegistration { return registrationFor(id, "native-1") })
	if err != nil {
		t.Fatal(err)
	}
	refused := errors.New("refused")
	found, err := local.UpdateRegistration(reg.ArchiveSessionID, func(r *archive.SessionRegistration) error {
		r.TranscriptPath = "/changed.jsonl"
		return refused
	})
	if !found || !errors.Is(err, refused) {
		t.Fatalf("found=%t err=%v", found, err)
	}
	if current, _, _ := local.LoadRegistration(reg.ArchiveSessionID); current.TranscriptPath != "/t.jsonl" {
		t.Fatalf("a refused update was saved: %#v", current)
	}
	if err := local.ForgetSession(reg.ArchiveSessionID, "native-1"); err != nil {
		t.Fatal(err)
	}
	called := false
	found, err = local.UpdateRegistration(reg.ArchiveSessionID, func(*archive.SessionRegistration) error { called = true; return nil })
	if found || err != nil || called {
		t.Fatalf("found=%t err=%v called=%t", found, err, called)
	}
	if _, exists, _ := local.LoadRegistration(reg.ArchiveSessionID); exists {
		t.Fatal("a forgotten registration was written back")
	}
}

// RegisterNewSession found an index entry, then waited for that ID's lock
// while retention finished forgetting the old registration and removed the
// entry. Under the lock it sees the entry gone and registers under a fresh ID
// whose index entry it created, instead of reusing the dead one.
func TestRegisterNewSessionDoesNotReuseAnIndexEntryBeingForgotten(t *testing.T) {
	store := newTestStore(t)
	old, err := store.RegisterNewSession("native-1", func(id string) archive.SessionRegistration { return registrationFor(id, "native-1") })
	if err != nil {
		t.Fatal(err)
	}
	release, err := local.NamedLock(store.home, requestLockName(old.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	// Retention has removed the registration but not yet the index entry.
	if err := os.Remove(store.registrationPath(old.ArchiveSessionID)); err != nil {
		t.Fatal(err)
	}
	done := make(chan archive.SessionRegistration, 1)
	failed := make(chan error, 1)
	go func() {
		reg, err := store.RegisterNewSession("native-1", func(id string) archive.SessionRegistration { return registrationFor(id, "native-1") })
		if err != nil {
			failed <- err
			return
		}
		done <- reg
	}()
	time.Sleep(100 * time.Millisecond)
	if err := store.ForgetSession(old.ArchiveSessionID, "native-1"); err != nil {
		t.Fatal(err)
	}
	release()
	var fresh archive.SessionRegistration
	select {
	case err := <-failed:
		t.Fatal(err)
	case fresh = <-done:
	}
	if fresh.ArchiveSessionID == old.ArchiveSessionID {
		t.Fatal("the forgotten archive ID was reused")
	}
	if id, found, _ := store.ArchiveSessionID("native-1"); !found || id != fresh.ArchiveSessionID {
		t.Fatalf("index = %q (found=%t), want %q", id, found, fresh.ArchiveSessionID)
	}
}

// ForgetSession unlinks the request lock file last. The lock is a flock on
// the file's inode, so a hook that opened the file just before the unlink can
// still lock that stale inode once retention releases it, while another
// process locks the fresh file the path now names: two holders of "the"
// lock for the same archive ID. That is harmless only because every record
// the holders recheck was removed before the unlink, so neither can write
// anything for the forgotten ID, whichever inode it holds.
func TestStaleLockInodeAfterForgetCannotWriteForTheSession(t *testing.T) {
	store := newTestStore(t)
	old, err := store.RegisterNewSession("native-1", func(id string) archive.SessionRegistration { return registrationFor(id, "native-1") })
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(store.home, requestLockName(old.ArchiveSessionID))
	release, err := local.NamedLock(store.home, requestLockName(old.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	// A hook opened the lock file while retention held it, and has not yet
	// called flock: it holds a descriptor to the inode retention is about to
	// unlink.
	stale, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer stale.Close()
	if err := store.ForgetSession(old.ArchiveSessionID, "native-1"); err != nil {
		t.Fatal(err)
	}
	release()
	if err := syscall.Flock(int(stale.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("the stale inode should be lockable once retention released it: %v", err)
	}
	defer func() { _ = syscall.Flock(int(stale.Fd()), syscall.LOCK_UN) }()

	// With the stale inode locked, every locked operation for the forgotten
	// ID still runs (it locks the fresh file) and every one of them refuses
	// to write.
	called := false
	found, err := store.UpdateRegistration(old.ArchiveSessionID, func(*archive.SessionRegistration) error { called = true; return nil })
	if found || err != nil || called {
		t.Fatalf("UpdateRegistration on a forgotten ID: found=%t err=%v called=%t", found, err, called)
	}
	if err := store.SaveRequest(old.ArchiveSessionID, "stop", time.Now()); !errors.Is(err, ErrSessionNotRegistered) {
		t.Fatalf("SaveRequest on a forgotten ID: %v", err)
	}
	fresh, err := store.RegisterNewSession("native-1", func(id string) archive.SessionRegistration { return registrationFor(id, "native-1") })
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ArchiveSessionID == old.ArchiveSessionID {
		t.Fatal("the forgotten archive ID was reused")
	}
	if _, exists, _ := store.LoadRegistration(old.ArchiveSessionID); exists {
		t.Fatal("something wrote a registration for the forgotten ID")
	}
	if _, exists, _ := store.LoadRequest(old.ArchiveSessionID); exists {
		t.Fatal("something wrote a request for the forgotten ID")
	}
}
