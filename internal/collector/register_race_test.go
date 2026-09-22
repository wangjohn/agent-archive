package collector

import (
	"errors"
	"os"
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
