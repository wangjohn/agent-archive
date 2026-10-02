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

type migrationDefect string

const (
	defectAgent   migrationDefect = "agent"
	defectNative  migrationDefect = "native"
	defectArchive migrationDefect = "archive"
	defectMissing migrationDefect = "missing"
	defectCorrupt migrationDefect = "corrupt"
	defectVersion migrationDefect = "version"
	defectUTF8    migrationDefect = "utf8"
)

func TestQualifiedLookupRejectsWrongReferencedOwner(t *testing.T) {
	for _, defect := range []migrationDefect{defectAgent, defectNative, defectArchive, defectMissing, defectCorrupt, defectVersion, defectUTF8} {
		t.Run(string(defect), func(t *testing.T) {
			s := newTestStore(t)
			key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "s"}
			reg := migrationRegistration(key, "old")
			entry := indexEntry(key, "old")
			switch defect {
			case defectAgent:
				reg.Harness.Name = "claude"
			case defectNative:
				reg.NativeSessionID = " S "
			case defectArchive:
				reg.ArchiveSessionID = "other"
			case defectVersion:
				entry.Version = 2
			case defectMissing, defectCorrupt:
			// These defects are applied to the persisted files below.
			case defectUTF8:
				key.NativeID = string([]byte{0xff})
			}
			if defect != defectMissing {
				if err := local.Write(s.registrationPath("old"), reg); err != nil {
					t.Fatal(err)
				}
			}
			if err := local.Write(qualifiedSessionIndexPath(s.home, key), entry); err != nil {
				t.Fatal(err)
			}
			if defect == defectCorrupt {
				writeCorrupt(t, s.registrationPath("old"))
			}
			if _, found, err := s.ArchiveSessionID(key); found || (err == nil && defect != defectMissing) {
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
	for _, tc := range []struct {
		boundary   string
		incomplete bool
	}{
		{"recovery-begin", false}, {"recovery-incomplete", true}, {"recovery-enumerated", true}, {"recovery-entry", true}, {"recovery-completing", true}, {"recovery-complete", false},
	} {
		boundary := tc.boundary
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
			//lint:ignore LV1001 boundary names match the string-valued durable interruption seam
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

func TestQualifiedRecoveryPreservesLegacyChildReservation(t *testing.T) {
	s := newTestStore(t)
	parentKey := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "parent"}
	childKey := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "parent:subagent:child"}
	legacyRegistration(t, s, parentKey, "parent-archive")
	if err := local.Write(nativeSessionIndexPath(s.home, childKey.NativeID), sessionIndexEntry{ArchiveSessionID: "child-archive"}); err != nil {
		t.Fatal(err)
	}
	candidate := SubagentCandidate{ArchiveSessionID: "child-archive", NativeSessionID: childKey.NativeID, ParentArchiveSessionID: "parent-archive", ParentNativeSessionID: parentKey.NativeID, ProjectID: "p", ProjectRoot: "/synthetic", Harness: archive.Harness{Name: "claude-code"}, AgentID: "child", TranscriptPath: "/synthetic/child.jsonl", ObservedAt: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)}
	if err := s.SaveSubagentCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	// A hook can arrive before the collector's first migration pass. It must
	// defer this legacy candidate, rather than reserve a second child identity.
	if id, created, err := s.EnsureArchiveSessionID(childKey); !errors.Is(err, ErrSessionIndexRecoveryRequired) || created || id != "" {
		t.Fatalf("legacy candidate reassigned before recovery: %q %t %v", id, created, err)
	}
	// Known corruption is requested before maintenance; its negative census must
	// not discard the candidate's stronger durable positive reservation evidence.
	if err := s.RequestSessionIndexRecovery(childKey); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverSessionIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.ArchiveSessionID(childKey); err != nil || found {
		t.Fatalf("candidate admitted %t %v", found, err)
	}
	if id, created, err := s.EnsureArchiveSessionID(childKey); err != nil || created || id != "child-archive" {
		t.Fatalf("candidate reassigned %q %t %v", id, created, err)
	}
	reg := migrationRegistration(childKey, "child-archive")
	reg.ParentSessionID, reg.ParentNativeSessionID, reg.SubagentID = "parent-archive", parentKey.NativeID, "child"
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if id, found, err := s.ArchiveSessionID(childKey); err != nil || !found || id != "child-archive" {
		t.Fatalf("child commit %q %t %v", id, found, err)
	}
}

func TestQualifiedRegistrationUpdateAndRemovalCannotChangeOwner(t *testing.T) {
	s := newTestStore(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "owner"}
	reg, err := s.RegisterNewSession(key, func(id string) archive.SessionRegistration { return migrationRegistration(key, id) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateRegistration(reg.ArchiveSessionID, func(r *archive.SessionRegistration) error { r.NativeSessionID = "other"; return nil }); !errors.Is(err, ErrSessionIdentityConflict) {
		t.Fatalf("identity update %v", err)
	}
	wrong := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: key.NativeID}
	if err := s.ForgetSession(reg.ArchiveSessionID, wrong); !errors.Is(err, ErrSessionIdentityConflict) {
		t.Fatalf("wrong removal %v", err)
	}
	if id, found, err := s.ArchiveSessionID(key); err != nil || !found || id != reg.ArchiveSessionID {
		t.Fatalf("owner lost %q %t %v", id, found, err)
	}
}

// A newer hook request cannot be certified by an earlier recovery census.
// Regression: phase 3a P3A-R1.
func TestQualifiedRecoveryKeepsRequestDuringCompletion(t *testing.T) {
	for _, duringSync := range []bool{false, true} {
		t.Run(map[bool]string{false: "before stage", true: "staged before commit"}[duringSync], func(t *testing.T) {
			s := newTestStore(t)
			key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "arriving-at-completion"}
			requested := false
			request := func() error {
				if requested {
					return nil
				}
				requested = true
				unlock, err := local.NamedLock(s.home, "hooks.lock")
				if err != nil {
					return err
				}
				defer unlock()
				return s.RequestSessionIndexRecovery(key)
			}
			s.onIndexStep = func(step string) error {
				if step == "recovery-completing" {
					if duringSync {
						s.onWriteSync = func() {
							if err := request(); err != nil {
								t.Fatal(err)
							}
						}
						return nil
					}
					return request()
				}
				return nil
			}
			err := s.RecoverSessionIndex(context.Background())
			if !requested {
				t.Fatal("request boundary not reached")
			}
			if !errors.Is(err, ErrSessionIndexRecoveryRequired) {
				t.Fatalf("newer generation accepted: %v", err)
			}
			s.onIndexStep, s.onWriteSync = nil, nil
			if err := s.RecoverSessionIndexIfNeeded(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, created, err := s.EnsureArchiveSessionID(key); err != nil || !created {
				t.Fatalf("newer request stranded by completed census: created=%t err=%v", created, err)
			}
		})
	}
}

func TestQualifiedRecoveryAcceptsExistingMarkerWithoutGeneration(t *testing.T) {
	t.Parallel()
	for _, complete := range []bool{false, true} {
		s := newTestStore(t)
		if err := local.Write(filepath.Join(s.home, sessionIndexMarkerFile), sessionIndexMarker{Version: 1, Complete: complete}); err != nil {
			t.Fatal(err)
		}
		s.onWriteSync = func() {
			unlock, err := local.NamedLock(s.home, "hooks.lock")
			if err != nil {
				t.Fatalf("marker sync held hooks lock: %v", err)
			}
			unlock()
		}
		if err := s.RecoverSessionIndexIfNeeded(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := s.sessionIndexMissAllowed(); err != nil {
			t.Fatal(err)
		}
	}
}

// Forgetting never leaves a live registration after its index was removed.
// Regression: phase 3a P3A-R2.
func TestQualifiedForgetInterruptionCannotLeaveUnindexedRegistration(t *testing.T) {
	s := newTestStore(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "forgotten"}
	reg, err := s.RegisterNewSession(key, func(id string) archive.SessionRegistration { return migrationRegistration(key, id) })
	if err != nil {
		t.Fatal(err)
	}
	interrupted := errors.New("interrupted after index removal")
	s.onIndexStep = func(step string) error {
		if step == "forget-indexes" {
			return interrupted
		}
		return nil
	}
	if err := s.ForgetSession(reg.ArchiveSessionID, key); !errors.Is(err, interrupted) {
		t.Fatalf("interruption: %v", err)
	}
	s.onIndexStep = nil
	if _, found, err := s.LoadRegistration(reg.ArchiveSessionID); err != nil || found {
		t.Fatalf("index disappeared before registration: found=%t err=%v", found, err)
	}
	fresh, err := s.RegisterNewSession(key, func(id string) archive.SessionRegistration { return migrationRegistration(key, id) })
	if err != nil || fresh.ArchiveSessionID == reg.ArchiveSessionID {
		t.Fatalf("fresh retry: %#v %v", fresh, err)
	}
	regs, err := s.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("orphan duplicate: %#v %v", regs, err)
	}
}
