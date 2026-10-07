package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
)

func TestSessionDeletionRemovesIndividuallyOwnedStagesAndOriginalEvidence(t *testing.T) {
	s, reg, b := stageFixture(t)
	digest, err := s.PrepareAdmissionStage(reg, b, "none", reg.Admitted())
	if err != nil {
		t.Fatal(err)
	}
	reg.AdmissionStage = digest
	if err = s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveAdmissionRequest(reg); err != nil {
		t.Fatal(err)
	}
	if err = s.SavePublicationEvidence(reg.ArchiveSessionID, []byte("synthetic original bytes")); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(s.home, publicationEvidenceDir, reg.ArchiveSessionID, "orphan.tmp"), []byte("durable orphan"), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := agentmeta.NewSessionKey(reg.Harness.Name, reg.NativeSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ForgetIdleSession(reg.ArchiveSessionID, key, false, &RemovalRecord{Harness: reg.Harness.Name, Reason: RemovalReasonUndo, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(s.home, admissionStageDir, reg.ArchiveSessionID+".source.gz"), filepath.Join(s.home, admissionStageDir, reg.ArchiveSessionID+".json"), filepath.Join(s.home, publicationEvidenceDir, reg.ArchiveSessionID)} {
		if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("explicitly removed evidence remains", path, err)
		}
	}
	if usage, err := s.admissionStageUsage(); err != nil || usage != deletionControlAllowance {
		t.Fatal("removed evidence still charged", usage, err)
	}
}

func TestSessionDeletionCorruptJournalNeverForgetsAdmission(t *testing.T) {
	s, reg, b := stageFixture(t)
	digest, err := s.PrepareAdmissionStage(reg, b, "none", reg.Admitted())
	if err != nil {
		t.Fatal(err)
	}
	reg.AdmissionStage = digest
	if err = s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PrepareSessionDeletion(reg, RemovalReasonUndo, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	path, _ := s.deletionPath(reg.ArchiveSessionID)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err = json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	value["phase"] = DeletionCleaned
	raw, _ = json.Marshal(value)
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	key, _ := agentmeta.NewSessionKey(reg.Harness.Name, reg.NativeSessionID)
	if err = s.ForgetSession(reg.ArchiveSessionID, key); !errors.Is(err, ErrAdmissionStageRecovery) {
		t.Fatal(err)
	}
	if _, found, err := s.LoadRegistration(reg.ArchiveSessionID); err != nil || !found {
		t.Fatal("corrupt journal lost owner", found, err)
	}
	if _, _, err = s.ReadAdmissionStage(reg.ArchiveSessionID, digest); err != nil {
		t.Fatal("corrupt journal lost staged evidence", err)
	}
}

func TestSessionDeletionAdmittedStageDefersExpiryWithoutRequest(t *testing.T) {
	s, reg, b := stageFixture(t)
	digest, err := s.PrepareAdmissionStage(reg, b, "none", reg.Admitted())
	if err != nil {
		t.Fatal(err)
	}
	reg.AdmissionStage = digest
	if err = s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	key, _ := agentmeta.NewSessionKey(reg.Harness.Name, reg.NativeSessionID)
	if forgotten, err := s.ForgetIdleSession(reg.ArchiveSessionID, key, true, &RemovalRecord{Harness: reg.Harness.Name, Reason: RemovalReasonRetention, At: time.Now()}); err != nil || forgotten {
		t.Fatal("stage expired", forgotten, err)
	}
}

func TestSessionDeletionInterruptedRootedCleanupRetainsOwnerAndResumes(t *testing.T) {
	s, reg, b := stageFixture(t)
	digest, err := s.PrepareAdmissionStage(reg, b, "none", reg.Admitted())
	if err != nil {
		t.Fatal(err)
	}
	reg.AdmissionStage = digest
	if err = s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err = s.SavePublicationEvidence(reg.ArchiveSessionID, []byte("owned evidence")); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "private")
	if err = os.WriteFile(outsideFile, []byte("unrelated bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PrepareSessionDeletion(reg, RemovalReasonUndo, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []SessionDeletionPhase{DeletionDeleting, DeletionAbsent, DeletionCleaned} {
		if err = s.AdvanceSessionDeletion(reg, phase); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(s.home, publicationEvidenceDir, reg.ArchiveSessionID, "corrupt-link")
	if err = os.Symlink(outsideFile, link); err != nil {
		t.Fatal(err)
	}
	key, _ := agentmeta.NewSessionKey(reg.Harness.Name, reg.NativeSessionID)
	if err = s.ForgetSession(reg.ArchiveSessionID, key); !errors.Is(err, ErrAdmissionStageRecovery) {
		t.Fatal("corrupt evidence was forgotten", err)
	}
	if _, found, err := s.LoadRegistration(reg.ArchiveSessionID); err != nil || !found {
		t.Fatal("interrupted cleanup lost owner", err)
	}
	if raw, err := os.ReadFile(outsideFile); err != nil || string(raw) != "unrelated bytes" {
		t.Fatal("cleanup followed corrupt link", err)
	}
	if journal, found, err := s.LoadSessionDeletion(reg); err != nil || !found || journal.Phase != DeletionCleaned {
		t.Fatal("cleanup authority not durable", journal, err)
	}
	if err = os.Remove(link); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(s.home)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.ForgetSession(reg.ArchiveSessionID, key); err != nil {
		t.Fatal("rooted cleanup did not resume", err)
	}
	if _, found, err := restarted.LoadRegistration(reg.ArchiveSessionID); err != nil || found {
		t.Fatal("removed owner remains", err)
	}
}

func TestSessionDeletionCleansOnlyOwnedScratchReservations(t *testing.T) {
	s, reg, _ := stageFixture(t)
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	owned, err := NewTemporaryReservation(s, CursorAdmission, reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owned.Close() }()
	other, err := NewTemporaryReservation(s, PublicationPrivacy, "other-owner")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	for _, reservation := range []*TemporaryReservation{owned, other} {
		if err = reservation.Reserve(1024); err != nil {
			t.Fatal(err)
		}
		if err = os.MkdirAll(reservation.Root(), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(reservation.Root(), "synthetic-snapshot"), []byte("filtered scratch"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	key, _ := agentmeta.NewSessionKey(reg.Harness.Name, reg.NativeSessionID)
	if err = s.ForgetSession(reg.ArchiveSessionID, key); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(owned.Root()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned scratch survived", err)
	}
	if raw, err := os.ReadFile(filepath.Join(other.Root(), "synthetic-snapshot")); err != nil || string(raw) != "filtered scratch" {
		t.Fatal("unrelated scratch changed", err)
	}
	if used, err := s.temporaryUsage(); err != nil || used != 1024+temporaryControlBytes {
		t.Fatal("unrelated charge lost", used, err)
	}
}

func TestSessionDeletionTerminalBookkeepingAndLostOwnerRecovery(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "crash lost registration", true: "complete local removal"}[completed], func(t *testing.T) {
			s, reg, _ := stageFixture(t)
			if err := s.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			if completed {
				key, _ := agentmeta.NewSessionKey(reg.Harness.Name, reg.NativeSessionID)
				if err := s.ForgetSession(reg.ArchiveSessionID, key); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := s.PrepareSessionDeletion(reg, RemovalReasonUndo, nil, time.Now()); err != nil {
					t.Fatal(err)
				}
				for _, phase := range []SessionDeletionPhase{DeletionDeleting, DeletionAbsent, DeletionCleaned} {
					if err := s.AdvanceSessionDeletion(reg, phase); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Remove(s.registrationPath(reg.ArchiveSessionID)); err != nil {
					t.Fatal(err)
				}
			}
			restarted, err := Open(s.home)
			if err != nil {
				t.Fatal(err)
			}
			journal, found, err := restarted.LoadSessionDeletion(reg)
			if err != nil || !found || journal.LocalRemoved != completed {
				t.Fatal("terminal proof changed", journal, err)
			}
			orphans, err := restarted.OrphanedSessions(nil)
			if err != nil || (len(orphans) == 0) != completed {
				t.Fatal("terminal versus lost-owner status incorrect", orphans, err)
			}
			if err := restarted.DeletionCaptureAllowed(reg, Request{Token: "new-stale-hook", Reasons: []string{"stop"}, RequestedAt: time.Now().Add(time.Hour)}); !errors.Is(err, ErrAdmissionStageRecovery) && !errors.Is(err, ErrRemovalPending) {
				t.Fatal("tombstone permitted stale resurrection", err)
			}
			if completed {
				if err := os.WriteFile(s.pendingPath(reg.ArchiveSessionID), []byte("corrupt newly surviving state"), 0600); err != nil {
					t.Fatal(err)
				}
				orphans, err = restarted.OrphanedSessions(nil)
				if err != nil || len(orphans) != 1 {
					t.Fatal("terminal flag hid newly surviving evidence", orphans, err)
				}
			}
		})
	}
}

func TestDeletionTemporaryOnlyOrphanAndTerminalRemainActionable(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "temporary only", true: "terminal with temporary"}[terminal], func(t *testing.T) {
			s, reg, _ := stageFixture(t)
			if terminal {
				if err := s.SaveRegistration(reg); err != nil {
					t.Fatal(err)
				}
				key, _ := agentmeta.NewSessionKey(reg.Harness.Name, reg.NativeSessionID)
				if err := s.ForgetSession(reg.ArchiveSessionID, key); err != nil {
					t.Fatal(err)
				}
			} else if err := os.MkdirAll(filepath.Join(s.home, "session-deletions"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(s.home, "session-deletions", reg.ArchiveSessionID+"-0123456789abcdef0123456789abcdef.tmp"), []byte("unfinished control"), 0600); err != nil {
				t.Fatal(err)
			}
			orphans, err := s.OrphanedSessions(nil)
			if err != nil || len(orphans) != 1 || orphans[0] != reg.ArchiveSessionID {
				t.Fatal("unfinished deletion hidden", orphans, err)
			}
		})
	}
}

func TestLocalDeletionSyncFailureCannotMarkTerminal(t *testing.T) {
	s, reg, _ := stageFixture(t)
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("synthetic nested removal sync failure")
	s.onLocalDeletionSync = func(name string) error {
		if name == "requests" {
			return injected
		}
		return nil
	}
	key, _ := agentmeta.NewSessionKey(reg.Harness.Name, reg.NativeSessionID)
	if err := s.ForgetSession(reg.ArchiveSessionID, key); !errors.Is(err, injected) {
		t.Fatal("undurable local removal reported finished", err)
	}
	j, found, err := s.LoadSessionDeletion(reg)
	if err != nil || !found || j.LocalRemoved {
		t.Fatal("failed nested sync created terminal proof", j, err)
	}
	orphans, err := s.OrphanedSessions(nil)
	if err != nil || len(orphans) != 1 {
		t.Fatal("failed nested sync hid recovery", orphans, err)
	}
}

func TestSessionDeletionPreservesStageOfDottedOtherOwner(t *testing.T) {
	s, reg, b := stageFixture(t)
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	other := reg
	other.ArchiveSessionID += ".other"
	other.NativeSessionID += "-other"
	b.ArchiveSessionID = other.ArchiveSessionID
	b.NativeSessionID = other.NativeSessionID
	digest, err := s.PrepareAdmissionStage(other, b, "none", other.Admitted())
	if err != nil {
		t.Fatal(err)
	}
	key, _ := agentmeta.NewSessionKey(reg.Harness.Name, reg.NativeSessionID)
	if err = s.ForgetSession(reg.ArchiveSessionID, key); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.ReadAdmissionStage(other.ArchiveSessionID, digest); err != nil {
		t.Fatal("unrelated owner stage removed", err)
	}
}

func TestDeletionControlRequiresCanonicalEncoding(t *testing.T) {
	s, reg, _ := stageFixture(t)
	if _, err := s.PrepareSessionDeletion(reg, RemovalReasonUndo, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	path, _ := s.deletionPath(reg.ArchiveSessionID)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.LoadSessionDeletion(reg); !found || !errors.Is(err, ErrAdmissionStageRecovery) {
		t.Fatal("noncanonical authority accepted", found, err)
	}
}

func TestSessionDeletionCorruptScratchSymlinkPreservesEvidenceAndOwner(t *testing.T) {
	s, reg, _ := stageFixture(t)
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	r, err := NewTemporaryReservation(s, CursorAdmission, reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if err = r.Reserve(1024); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(r.Root(), 0700); err != nil {
		t.Fatal(err)
	}
	// Corruption occurs after remote cleanup already made the intent cleaned.
	// No new quota scan should mask the owning cleanup regression.
	if _, err := s.PrepareSessionDeletion(reg, RemovalReasonUndo, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []SessionDeletionPhase{DeletionDeleting, DeletionAbsent, DeletionCleaned} {
		if err := s.AdvanceSessionDeletion(reg, phase); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "unrelated")
	if err = os.WriteFile(outside, []byte("unrelated bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(r.Root(), "corrupt")); err != nil {
		t.Fatal(err)
	}
	key, _ := agentmeta.NewSessionKey(reg.Harness.Name, reg.NativeSessionID)
	if err = s.ForgetSession(reg.ArchiveSessionID, key); !errors.Is(err, ErrAdmissionStageRecovery) {
		t.Fatal("corrupt scratch silently forgotten", err)
	}
	if _, found, err := s.LoadRegistration(reg.ArchiveSessionID); err != nil || !found {
		t.Fatal("corrupt scratch lost owner", err)
	}
	if body, err := os.ReadFile(outside); err != nil || string(body) != "unrelated bytes" {
		t.Fatal("outside target changed", err)
	}
}

func (s *Store) deletionPath(id string) (string, error) {
	if err := validateDeletionID(id); err != nil {
		return "", err
	}
	return filepath.Join(s.home, "session-deletions", id+".json"), nil
}

func TestDeletionWritePinsQuotaAndControlAcrossHomeReplacement(t *testing.T) {
	s, reg, _ := stageFixture(t)
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	original := s.home + "-held"
	s.onDeletionBeforeCommit = func() error {
		if err := os.Rename(s.home, original); err != nil {
			return err
		}
		return os.Mkdir(s.home, 0700)
	}
	t.Cleanup(func() { _ = os.RemoveAll(original) })
	if _, err := s.PrepareSessionDeletion(reg, RemovalReasonUndo, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(original, "session-deletions", reg.ArchiveSessionID+".json")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(s.home)
	if err != nil || len(entries) != 0 {
		t.Fatal("held write touched replacement root", entries, err)
	}
	if err := s.AdvanceSessionDeletion(reg, DeletionDeleting); !errors.Is(err, ErrAdmissionStageRecovery) {
		t.Fatal("replacement root inherited deletion authority", err)
	}
}

func TestSessionDeletionRefusesSymlinkedListingStateBeforeUnlink(t *testing.T) {
	s, reg, _ := stageFixture(t)
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	file := filepath.Join(outside, reg.ArchiveSessionID+".json")
	if err := os.WriteFile(file, []byte("unrelated listing state"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.home, listingRepairDir)); err != nil {
		t.Fatal(err)
	}
	key, _ := agentmeta.NewSessionKey(reg.Harness.Name, reg.NativeSessionID)
	if err := s.ForgetSession(reg.ArchiveSessionID, key); err == nil {
		t.Fatal("symlinked cleanup reported success")
	}
	if raw, err := os.ReadFile(file); err != nil || string(raw) != "unrelated listing state" {
		t.Fatal("cleanup removed unrelated symlink target", err)
	}
	if _, found, err := s.LoadRegistration(reg.ArchiveSessionID); err != nil || !found {
		t.Fatal("corrupt cleanup lost owner", found, err)
	}
}

func TestTerminalDeletionSurvivingScratchReceiptRemainsActionable(t *testing.T) {
	s, reg, _ := stageFixture(t)
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	key, _ := agentmeta.NewSessionKey(reg.Harness.Name, reg.NativeSessionID)
	if err := s.ForgetSession(reg.ArchiveSessionID, key); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.OrphanedSessions(nil); err != nil || len(ids) != 0 {
		t.Fatal(ids, err)
	}
	r, err := NewTemporaryReservation(s, PublicationPrivacy, reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if err := r.Reserve(1024); err != nil {
		t.Fatal(err)
	}
	ids, err := s.OrphanedSessions(nil)
	if err != nil || len(ids) != 1 || ids[0] != reg.ArchiveSessionID {
		t.Fatal("terminal marker hid surviving scratch obligation", ids, err)
	}
}

func TestTerminalDeletionCorruptRecordDirectoryRemainsActionable(t *testing.T) {
	for _, name := range []string{"pending", "sessions/session", temporaryReservationDir, temporaryScratchDir} {
		t.Run(name, func(t *testing.T) {
			s, reg, _ := stageFixture(t)
			if err := s.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			key, _ := agentmeta.NewSessionKey(reg.Harness.Name, reg.NativeSessionID)
			if err := s.ForgetSession(reg.ArchiveSessionID, key); err != nil {
				t.Fatal(err)
			}
			if name == "sessions/session" {
				name = filepath.Join("sessions", reg.ArchiveSessionID)
			}
			path := filepath.Join(s.home, name)
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), path); err != nil {
				t.Fatal(err)
			}
			ids, err := s.OrphanedSessions(nil)
			if err != nil || len(ids) != 1 {
				t.Fatal("terminal marker hid corrupt record directory", ids, err)
			}
		})
	}
}

func TestSessionDeletionPinsRecordsAcrossDirectoryReplacement(t *testing.T) {
	s, reg, _ := stageFixture(t)
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(s.home, listingRepairDir)
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	name := reg.ArchiveSessionID + ".json"
	if err := os.WriteFile(filepath.Join(directory, name), []byte("owned listing state"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, name), []byte("unrelated state"), 0600); err != nil {
		t.Fatal(err)
	}
	s.onDeletionBeforeCommit = func() error {
		s.onDeletionBeforeCommit = nil
		if err := os.Rename(directory, directory+"-held"); err != nil {
			return err
		}
		return os.Symlink(outside, directory)
	}
	key, _ := agentmeta.NewSessionKey(reg.Harness.Name, reg.NativeSessionID)
	if err := s.ForgetSession(reg.ArchiveSessionID, key); err == nil {
		t.Fatal("replaced directory reported terminal success")
	}
	if raw, err := os.ReadFile(filepath.Join(outside, name)); err != nil || string(raw) != "unrelated state" {
		t.Fatal("replacement target was unlinked", err)
	}
	if j, found, err := s.LoadSessionDeletion(reg); err != nil || !found || j.LocalRemoved {
		t.Fatal("replaced directory granted terminal proof", j, found, err)
	}
}
