package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

func migrationRegistration(key agentmeta.SessionKey, id string) archive.SessionRegistration {
	return archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: key.NativeID, ProjectID: "p", ProjectRoot: "/synthetic", Harness: archive.Harness{Name: string(key.Agent)}, TranscriptPath: "/synthetic/t.jsonl", SessionStartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func legacyRegistration(t *testing.T, s *Store, key agentmeta.SessionKey, id string) {
	t.Helper()
	reg := migrationRegistration(key, id)
	if err := local.Write(s.registrationPath(id), reg); err != nil {
		t.Fatal(err)
	}
	if err := local.Write(nativeSessionIndexPath(s.home, key.NativeID), sessionIndexEntry{ArchiveSessionID: id}); err != nil {
		t.Fatal(err)
	}
}

func TestQualifiedLegacyAdoptionAndCrossAgentCollision(t *testing.T) {
	s := newTestStore(t)
	claude := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: " ABC /\x00界 "}
	legacyRegistration(t, s, claude, "historical")
	reg, _, _ := s.LoadRegistration("historical")
	reg.Harness.Name = "CLAUDE-CODE"
	if err := local.Write(s.registrationPath("historical"), reg); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []agentmeta.ID{agentmeta.Codex, agentmeta.Cursor} {
		key := agentmeta.SessionKey{Agent: agent, NativeID: claude.NativeID}
		if id, found, err := s.ArchiveSessionID(key); err != nil || found || id != "" {
			t.Fatalf("wrong-agent adopted %q %t %v", id, found, err)
		}
		created, err := s.RegisterNewSession(key, func(id string) archive.SessionRegistration { return migrationRegistration(key, id) })
		if err != nil || created.ArchiveSessionID == "historical" {
			t.Fatalf("collision %#v %v", created, err)
		}
	}
	id, created, err := s.EnsureArchiveSessionID(claude)
	if err != nil || created || id != "historical" {
		t.Fatalf("adoption %q %t %v", id, created, err)
	}
	reload := OpenReadOnly(s.home)
	if id, found, err := reload.ArchiveSessionID(claude); err != nil || !found || id != "historical" {
		t.Fatalf("reload %q %t %v", id, found, err)
	}
	var legacy sessionIndexEntry
	if err := local.Read(nativeSessionIndexPath(s.home, claude.NativeID), &legacy); err != nil || legacy.ArchiveSessionID != "historical" {
		t.Fatalf("legacy changed %#v %v", legacy, err)
	}
	if reg, _, _ := s.LoadRegistration("historical"); reg.Harness.Name != "CLAUDE-CODE" {
		t.Fatal("registration rewritten")
	}
}

func TestQualifiedLookupRejectsWrongReferencedOwner(t *testing.T) {
	for _, defect := range []string{"agent", "native", "archive", "missing", "corrupt", "version", "utf8"} {
		t.Run(defect, func(t *testing.T) {
			s := newTestStore(t)
			key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "s"}
			reg := migrationRegistration(key, "old")
			entry := indexEntry(key, "old")
			switch defect {
			case "agent":
				reg.Harness.Name = "claude"
			case "native":
				reg.NativeSessionID = " S "
			case "archive":
				reg.ArchiveSessionID = "other"
			case "version":
				entry.Version = 2
			case "utf8":
				key.NativeID = string([]byte{0xff})
			}
			if defect != "missing" {
				if err := local.Write(s.registrationPath("old"), reg); err != nil {
					t.Fatal(err)
				}
			}
			if err := local.Write(qualifiedSessionIndexPath(s.home, key), entry); err != nil {
				t.Fatal(err)
			}
			if defect == "corrupt" {
				writeCorrupt(t, s.registrationPath("old"))
			}
			if _, found, err := s.ArchiveSessionID(key); found || (err == nil && defect != "missing") {
				t.Fatalf("accepted defect found=%t err=%v", found, err)
			}
		})
	}
}

func TestQualifiedLookupNeverScansRegistrations(t *testing.T) {
	s := newTestStore(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "new"}
	// An unreadable enumeration target would fail a full scan, but bounded
	// ordinary miss never even touches the registration directory.
	if err := os.Remove(filepath.Join(s.home, "registrations")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.home, "registrations"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.ArchiveSessionID(key); err != nil || found {
		t.Fatalf("miss scanned %t %v", found, err)
	}
	writeCorrupt(t, qualifiedSessionIndexPath(s.home, key))
	if _, _, err := s.ArchiveSessionID(key); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		t.Fatalf("corruption scanned: %v", err)
	}
}

func TestQualifiedRegistrationDurableInterruption(t *testing.T) {
	for _, boundary := range []string{"reservation", "registration", "committed"} {
		t.Run(boundary, func(t *testing.T) {
			s := newTestStore(t)
			key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "fresh"}
			injected := errors.New("interrupted")
			s.onIndexStep = func(step string) error {
				if step == boundary {
					return injected
				}
				return nil
			}
			if _, err := s.RegisterNewSession(key, func(id string) archive.SessionRegistration { return migrationRegistration(key, id) }); !errors.Is(err, injected) {
				t.Fatalf("failure not injected: %v", err)
			}
			entry, present, err := s.readQualifiedIndex(key)
			if err != nil || !present {
				t.Fatalf("reservation absent %#v %t %v", entry, present, err)
			}
			retained := entry.ArchiveSessionID
			if _, found, err := s.ArchiveSessionID(key); err != nil || found != (boundary != "reservation") {
				t.Fatalf("public reservation admitted %t %v", found, err)
			}
			s = OpenReadOnly(s.home)
			reg, err := s.RegisterNewSession(key, func(id string) archive.SessionRegistration { return migrationRegistration(key, id) })
			if err != nil || reg.ArchiveSessionID != retained {
				t.Fatalf("retry changed ID %#v %v", reg, err)
			}
			regs, err := s.LoadRegistrations()
			if err != nil || len(regs) != 1 {
				t.Fatalf("duplicate orphan %#v %v", regs, err)
			}
		})
	}
}

func TestQualifiedAdoptionInterruptionAndExpiry(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(map[bool]string{false: "after durable adoption", true: "expiry during stage"}[expire], func(t *testing.T) {
			s := newTestStore(t)
			key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "old"}
			legacyRegistration(t, s, key, "historic")
			injected := errors.New("interrupted")
			if !expire {
				s.onIndexStep = func(step string) error {
					if step == "adoption" {
						return injected
					}
					return nil
				}
			} else {
				fired := false
				s.onIndexSync = func() {
					if fired {
						return
					}
					fired = true
					if err := s.ForgetSession("historic", key); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, _, err := s.EnsureArchiveSessionID(key)
			if !expire && !errors.Is(err, injected) {
				t.Fatalf("interruption %v", err)
			}
			if expire && !errors.Is(err, errIndexMoved) {
				t.Fatalf("expiry ignored: %v", err)
			}
			s.onIndexStep = nil
			s.onIndexSync = nil
			id, created, err := s.EnsureArchiveSessionID(key)
			if err != nil || created != expire || (id == "historic") == expire {
				t.Fatalf("retry %q %t %v", id, created, err)
			}
			if expire {
				if _, found, _ := s.LoadRegistration("historic"); found {
					t.Fatal("expired registration revived")
				}
			}
		})
	}
}

func TestQualifiedRecoveryConflictsAndIncompleteEnumeration(t *testing.T) {
	s := newTestStore(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "duplicate"}
	for _, id := range []string{"one", "two"} {
		if err := local.Write(s.registrationPath(id), migrationRegistration(key, id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecoverSessionIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ArchiveSessionID(key); !errors.Is(err, ErrSessionIdentityConflict) {
		t.Fatalf("picked duplicate %v", err)
	}
	if err := os.Remove(s.registrationPath("two")); err != nil {
		t.Fatal(err)
	}
	absent := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "absent"}
	if err := s.RequestSessionIndexRecovery(absent); err != nil {
		t.Fatal(err)
	}
	writeCorrupt(t, s.registrationPath("broken"))
	if err := s.RecoverSessionIndex(context.Background()); err == nil {
		t.Fatal("incomplete census succeeded")
	}
	if _, _, err := s.EnsureArchiveSessionID(absent); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		t.Fatalf("partial inventory proved absence: %v", err)
	}
	if err := os.Remove(s.registrationPath("broken")); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverSessionIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	if id, found, err := s.ArchiveSessionID(key); err != nil || !found || id != "one" {
		t.Fatalf("resolved conflict %q %t %v", id, found, err)
	}
	if _, created, err := s.EnsureArchiveSessionID(absent); err != nil || !created {
		t.Fatalf("complete absent %t %v", created, err)
	}
}

func TestQualifiedRecoveryResumesEveryDurableBoundary(t *testing.T) {
	for _, boundary := range []string{"recovery-begin", "recovery-incomplete", "recovery-enumerated", "recovery-entry", "recovery-completing", "recovery-complete"} {
		t.Run(boundary, func(t *testing.T) {
			s := newTestStore(t)
			key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "old"}
			legacyRegistration(t, s, key, "stable")
			interrupted := errors.New("interrupted")
			s.onIndexStep = func(step string) error {
				if step == boundary {
					return interrupted
				}
				return nil
			}
			if err := s.RecoverSessionIndex(context.Background()); !errors.Is(err, interrupted) {
				t.Fatalf("missing boundary %v", err)
			}
			if boundary != "recovery-begin" && boundary != "recovery-complete" {
				if err := s.sessionIndexMissAllowed(); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
					t.Fatalf("partial census marked complete %v", err)
				}
			}
			s.onIndexStep = nil
			if err := s.RecoverSessionIndex(context.Background()); err != nil {
				t.Fatal(err)
			}
			if id, found, err := s.ArchiveSessionID(key); err != nil || !found || id != "stable" {
				t.Fatalf("resume %q %t %v", id, found, err)
			}
		})
	}
}

func TestQualifiedForgetPreservesOtherAgentLegacyAndReplacement(t *testing.T) {
	s := newTestStore(t)
	claude := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "collision"}
	codex := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: claude.NativeID}
	legacyRegistration(t, s, claude, "claude-old")
	reg, err := s.RegisterNewSession(codex, func(id string) archive.SessionRegistration { return migrationRegistration(codex, id) })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetSession(reg.ArchiveSessionID, codex); err != nil {
		t.Fatal(err)
	}
	if id, found, err := s.ArchiveSessionID(claude); err != nil || !found || id != "claude-old" {
		t.Fatalf("deleted collision %q %t %v", id, found, err)
	}
	if err := local.Write(qualifiedSessionIndexPath(s.home, claude), indexEntry(claude, "replacement")); err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetSession("claude-old", claude); err != nil {
		t.Fatal(err)
	}
	entry, found, err := s.readQualifiedIndex(claude)
	if err != nil || !found || entry.ArchiveSessionID != "replacement" {
		t.Fatalf("deleted replacement %#v %t %v", entry, found, err)
	}
}

func TestQualifiedIndexSyncNeverHoldsRequestLock(t *testing.T) {
	s := newTestStore(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "fresh"}
	syncs := 0
	s.onIndexSync = func() {
		syncs++
		entries, err := os.ReadDir(filepath.Join(s.home, "request-locks"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			unlock, err := local.NamedLock(s.home, filepath.Join("request-locks", entry.Name()))
			if err != nil {
				t.Fatalf("sync held lock: %v", err)
			}
			unlock()
		}
	}
	if _, err := s.RegisterNewSession(key, func(id string) archive.SessionRegistration { return migrationRegistration(key, id) }); err != nil {
		t.Fatal(err)
	}
	if syncs != 4 {
		t.Fatalf("index syncs %d", syncs)
	}
}
