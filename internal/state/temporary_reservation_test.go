package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestTemporaryReservationPersistsAndCleansOwnedScratch(t *testing.T) {
	t.Parallel()
	s, reg, bundle := stageFixture(t)
	r, err := NewTemporaryReservation(s, CursorAdmission, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(r.Root()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("constructor allocated", err)
	}
	if err = r.Reserve(128 << 20); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(r.Root(), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(r.Root(), "snapshot"), []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	restarted := OpenReadOnly(s.Home())
	used, err := restarted.admissionStageUsage()
	if err != nil || used != 128<<20+temporaryControlBytes+deletionControlAllowance {
		t.Fatal(used, err)
	}
	duplicate, err := NewTemporaryReservation(restarted, CursorAdmission, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = duplicate.Close() }()
	if err = duplicate.Reserve(1); !errors.Is(err, ErrAdmissionStageRecovery) {
		t.Fatal("duplicate owner", err)
	}
	if _, err = s.PrepareAdmissionStage(reg, bundle, "none", reg.AdmittedAt); err != nil {
		t.Fatal(err)
	}
	r.Release(128 << 20)
	if !errors.Is(r.Err(), ErrAdmissionStageRecovery) {
		t.Fatal("released existing scratch")
	}
	if err = r.Close(); !errors.Is(err, ErrAdmissionStageRecovery) {
		t.Fatal("lost retained error", err)
	}
	if _, err = os.Stat(r.Root()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cleanup", err)
	}
	used, err = s.temporaryUsage()
	if err != nil || used != 0 {
		t.Fatal("charge after verified cleanup", used, err)
	}
}

func TestTemporaryReservationQuotaBeforeAllocationAndOrphans(t *testing.T) {
	t.Parallel()
	s, reg, bundle := stageFixture(t)
	r, err := NewTemporaryReservation(s, PublicationPrivacy, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if err = r.Reserve(AdmissionStageQuota - temporaryControlBytes - deletionControlAllowance); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PrepareAdmissionStage(reg, bundle, "none", reg.AdmittedAt); !errors.Is(err, ErrAdmissionStageCapacity) {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(s.Home(), admissionStageDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stage allocated despite capacity", err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(s.Home(), temporaryScratchDir, "orphan"), 0700); err != nil {
		t.Fatal(err)
	}
	next, err := NewTemporaryReservation(s, CursorAdmission, "next")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = next.Close() }()
	if err = next.Reserve(1); !errors.Is(err, ErrAdmissionStageRecovery) {
		t.Fatal("unreserved orphan ignored", err)
	}
}

func TestTemporaryReservationRootEscapeRetainsCharge(t *testing.T) {
	t.Parallel()
	s, _, _ := stageFixture(t)
	r, err := NewTemporaryReservation(s, CursorAdmission, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Reserve(1024); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	marker := filepath.Join(outside, "keep")
	if err = os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(s.Home(), temporaryScratchDir)); err != nil {
		t.Fatal(err)
	}
	if err = r.Close(); !errors.Is(err, ErrAdmissionStageRecovery) {
		t.Fatal(err)
	}
	if _, err = os.Stat(marker); err != nil {
		t.Fatal("removed outside file", err)
	}
	if _, err = os.Stat(filepath.Join(s.Home(), temporaryReservationDir, r.manifest.Token+".json")); err != nil {
		t.Fatal("lost retained charge", err)
	}
}

func TestTemporaryWorkspaceRequiresDurableReservationAndRootConfinement(t *testing.T) {
	t.Parallel()
	s, _, _ := stageFixture(t)
	r, err := NewTemporaryReservation(s, CursorAdmission, "rooted")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if root, e := r.OpenWorkspace(); e == nil {
		_ = root.Close()
		t.Fatal("workspace allocated without reservation")
	}
	if err = r.Reserve(4096); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	parent := filepath.Join(s.Home(), temporaryScratchDir)
	if err = os.Symlink(outside, parent); err != nil {
		t.Fatal(err)
	}
	if root, e := r.OpenWorkspace(); e == nil {
		_ = root.Close()
		t.Fatal("workspace escaped held home")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("outside allocated", entries, err)
	}
	if err = os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	root, err := r.OpenWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	if err = root.Close(); err != nil {
		t.Fatal(err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTemporaryReservationAncestorSwapKeepsAccountingInHeldHome(t *testing.T) {
	t.Parallel()
	s, _, _ := stageFixture(t)
	r, err := NewTemporaryReservation(s, CursorAdmission, "held-home")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	original := s.Home()
	retained := original + "-retained"
	outside := t.TempDir()
	if err = os.Rename(original, retained); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(original); _ = os.Rename(retained, original) })
	if err = os.Symlink(outside, original); err != nil {
		t.Fatal(err)
	}
	if err = r.Reserve(4096); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("accounting escaped held home", entries, err)
	}
	if _, err = os.Stat(filepath.Join(retained, temporaryReservationDir, r.manifest.Token+".json")); err != nil {
		t.Fatal("held home has no durable reservation", err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTemporaryQuotaChargesOversizedOwnedScratchWithoutBodyReads(t *testing.T) {
	t.Parallel()
	s, _, _ := stageFixture(t)
	r, err := NewTemporaryReservation(s, CursorAdmission, "physical")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if err = r.Reserve(4096); err != nil {
		t.Fatal(err)
	}
	root, err := r.OpenWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	f, err := root.OpenFile("orphan", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(8192); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = root.Close(); err != nil {
		t.Fatal(err)
	}
	reads := 0
	s.onQuotaBodyRead = func(string) { reads++ }
	used, err := s.admissionStageUsage()
	if err != nil || used < temporaryControlBytes+8192 || reads != 0 {
		t.Fatal("physical scratch undercharged", used, reads, err)
	}
}

func TestTemporaryReleaseSyncFailureRestoresVisibleCharge(t *testing.T) {
	t.Parallel()
	s, _, _ := stageFixture(t)
	fault := errors.New("synthetic directory sync failure")
	s.onTemporaryRelease = func() error { return fault }
	r, err := NewTemporaryReservation(s, CursorAdmission, "sync-failure")
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Reserve(4096); err != nil {
		t.Fatal(err)
	}
	if err = r.Close(); !errors.Is(err, fault) {
		t.Fatal("lost cleanup failure", err)
	}
	used, err := OpenReadOnly(s.Home()).admissionStageUsage()
	if err != nil || used != 4096+temporaryControlBytes+deletionControlAllowance {
		t.Fatal("failed cleanup lost durable charge", used, err)
	}
}
