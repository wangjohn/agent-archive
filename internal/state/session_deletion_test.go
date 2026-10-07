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
	value["phase"] = "cleaned"
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
	for _, phase := range []string{"deleting", "absent", "cleaned"} {
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
	if journal, found, err := s.LoadSessionDeletion(reg); err != nil || !found || journal.Phase != "cleaned" {
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
				for _, phase := range []string{"deleting", "absent", "cleaned"} {
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
			if err := restarted.DeletionCaptureAllowed(reg, Request{Token: "new-stale-hook", Reasons: []string{"stop"}, RequestedAt: time.Now().Add(time.Hour)}); !errors.Is(err, ErrAdmissionStageRecovery) {
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
