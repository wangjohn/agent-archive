package state

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func fillReservedPool(t *testing.T, s *Store) *TemporaryReservation {
	t.Helper()
	r, err := NewTemporaryReservation(s, CursorAdmission, "capacity-owner")
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Reserve(AdmissionStageQuota - temporaryControlBytes - deletionControlAllowance); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}
func TestDeletionAllowanceInsideSaturatedPoolAndAtomicCoexistence(t *testing.T) {
	s, reg, _ := stageFixture(t)
	fillReservedPool(t, s)
	if used, err := s.admissionStageUsage(); err != nil || used != AdmissionStageQuota {
		t.Fatal(used, err)
	}
	if _, err := s.PrepareSessionDeletion(reg, RemovalReasonUndo, nil, time.Now()); err != nil {
		t.Fatal("reserved cleanup cannot make progress", err)
	}
	if err := s.writeDeletionFile(reg.ArchiveSessionID, make([]byte, deletionControlLimit)); err != nil {
		t.Fatal("bounded old/new coexistence rejected", err)
	}
	if err := s.writeDeletionFile(reg.ArchiveSessionID, make([]byte, deletionControlLimit)); err != nil {
		t.Fatal("replacement cannot use charged allowance", err)
	}
	if err := s.writeDeletionFile("second", make([]byte, deletionControlLimit)); err != nil {
		t.Fatal("whole-store allowance unusable", err)
	}
	if err := s.writeDeletionFile("third", make([]byte, deletionControlLimit)); !errors.Is(err, ErrAdmissionStageCapacity) {
		t.Fatal("multiple controls exceeded physical pool", err)
	}
	if actual, err := s.deletionControlUsage(); err != nil || actual != deletionControlAllowance {
		t.Fatal("failed write allocated bytes", actual, err)
	}
	if used, err := s.admissionStageUsage(); err != nil || used != AdmissionStageQuota {
		t.Fatal("pool cap changed", used, err)
	}
}
func TestDeletionIntentRefusesPreexistingFullPhysicalPool(t *testing.T) {
	s, reg, _ := stageFixture(t)
	f, err := os.Create(filepath.Join(s.home, admissionStageDir, "legacy-orphan.source.gz"))
	if errors.Is(err, os.ErrNotExist) {
		if err = os.MkdirAll(filepath.Join(s.home, admissionStageDir), 0700); err != nil {
			t.Fatal(err)
		}
		f, err = os.Create(filepath.Join(s.home, admissionStageDir, "legacy-orphan.source.gz"))
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(AdmissionStageQuota); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PrepareSessionDeletion(reg, RemovalReasonUndo, nil, time.Now()); !errors.Is(err, ErrAdmissionStageCapacity) {
		t.Fatal("invented cleanup space for existing full pool", err)
	}
	if _, err = os.Lstat(filepath.Join(s.home, "session-deletions")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed intent allocated control", err)
	}
	if info, err := os.Stat(filepath.Join(s.home, admissionStageDir, "legacy-orphan.source.gz")); err != nil || info.Size() != AdmissionStageQuota {
		t.Fatal("capacity failure evicted evidence", err)
	}
}
func TestDeletionAllowanceSerializesWithTemporaryReservation(t *testing.T) {
	s, reg, _ := stageFixture(t)
	r, err := NewTemporaryReservation(s, CursorAdmission, "concurrent")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	var wg sync.WaitGroup
	var allocateErr, intentErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		allocateErr = r.Reserve(AdmissionStageQuota - temporaryControlBytes - deletionControlAllowance)
	}()
	go func() {
		defer wg.Done()
		_, intentErr = s.PrepareSessionDeletion(reg, RemovalReasonUndo, nil, time.Now())
	}()
	wg.Wait()
	if allocateErr != nil || intentErr != nil {
		t.Fatal("shared reservation lock lost cleanup capacity", allocateErr, intentErr)
	}
	if used, err := s.admissionStageUsage(); err != nil || used != AdmissionStageQuota {
		t.Fatal(used, err)
	}
}
func TestDeletionControlAccountingIncludesCorruptOrphansWithoutBodyReads(t *testing.T) {
	s, _, _ := stageFixture(t)
	reads := 0
	s.onQuotaBodyRead = func(string) { reads++ }
	if used, err := s.admissionStageUsage(); err != nil || used != deletionControlAllowance || reads != 0 {
		t.Fatal("empty accounting performed body reads", used, reads, err)
	}
	dir := filepath.Join(s.home, "session-deletions")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"restored.json", "corrupt.json", "orphan.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, deletionControlLimit), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if used, err := s.admissionStageUsage(); err != nil || used != 3*deletionControlLimit || reads != 0 {
		t.Fatal("retained controls not conservatively charged", used, reads, err)
	}
}
