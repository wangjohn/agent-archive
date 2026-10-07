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
	if usage, err := s.admissionStageUsage(); err != nil || usage != 0 {
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
