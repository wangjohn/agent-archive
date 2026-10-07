package state

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPublicationEvidenceQuotaPrivateRestartAndReplacement(t *testing.T) {
	t.Parallel()
	s, _, _ := stageFixture(t)
	raw := []byte("synthetic original pending journal")
	if err := s.SavePublicationEvidence("synthetic", raw); err != nil {
		t.Fatal(err)
	}
	path, _ := s.evidencePath("synthetic")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	used, err := s.admissionStageUsage()
	if err != nil || used != deletionControlAllowance+publicationEvidenceControl+2*int64(len(raw)) {
		t.Fatal(used, err)
	}
	restarted := OpenReadOnly(s.Home())
	got, err := restarted.ReadPublicationEvidence("synthetic")
	if err != nil || !bytes.Equal(raw, got) {
		t.Fatal(string(got), err)
	}
	next := []byte("synthetic original with typed recovery state")
	if err = restarted.SavePublicationEvidence("synthetic", next); err != nil {
		t.Fatal(err)
	}
	got, err = restarted.ReadPublicationEvidence("synthetic")
	if err != nil || !bytes.Equal(next, got) {
		t.Fatal(string(got), err)
	}
	if err = restarted.RemovePublicationEvidence("synthetic"); err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.ReadPublicationEvidence("synthetic"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if used, err = restarted.admissionStageUsage(); err != nil || used != deletionControlAllowance {
		t.Fatal("cleanup not reflected", used, err)
	}
}

func TestPublicationEvidenceRefusesCapacityBeforeAllocationAndEscape(t *testing.T) {
	t.Parallel()
	s, _, _ := saturatedStagePending(t)
	if err := s.SavePublicationEvidence("synthetic", []byte("original")); !errors.Is(err, ErrAdmissionStageCapacity) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Home(), publicationEvidenceDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("allocated before quota", err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(s.Home(), publicationEvidenceDir)); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePublicationEvidence("synthetic", []byte("original")); !errors.Is(err, ErrAdmissionStageRecovery) {
		t.Fatal(err)
	}
	if _, err := s.ReadPublicationEvidence("synthetic"); !errors.Is(err, ErrAdmissionStageRecovery) {
		t.Fatal(err)
	}
	if err := s.RemovePublicationEvidence("synthetic"); !errors.Is(err, ErrAdmissionStageRecovery) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("escaped evidence root", entries, err)
	}
}

func TestPublicationEvidenceOrphansRemainChargedWithoutBodyReads(t *testing.T) {
	t.Parallel()
	s, _, _ := stageFixture(t)
	if err := s.SavePublicationEvidence("synthetic", []byte("original")); err != nil {
		t.Fatal(err)
	}
	path, _ := s.evidencePath("synthetic")
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "journal-orphan.tmp"), []byte("retained"), 0600); err != nil {
		t.Fatal(err)
	}
	reads := 0
	s.onQuotaBodyRead = func(string) { reads++ }
	if err := s.RemovePublicationEvidence("synthetic"); err != nil {
		t.Fatal(err)
	}
	used, err := s.admissionStageUsage()
	if err != nil || used != deletionControlAllowance+publicationEvidenceControl+2*int64(len("retained")) || reads != 0 {
		t.Fatal(used, reads, err)
	}
}
