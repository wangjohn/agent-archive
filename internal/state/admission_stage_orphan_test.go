package state

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSourceOnlyAdmissionStageRemainsActionableAfterRestart(t *testing.T) {
	s, reg, b := stageFixture(t)
	if _, err := s.PrepareAdmissionStage(reg, b, "none", reg.Admitted()); err != nil {
		t.Fatal(err)
	}
	// The source is durably written before its manifest; model that crash boundary.
	if err := os.Remove(filepath.Join(s.home, admissionStageDir, reg.ArchiveSessionID+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.home, admissionStageDir, reg.ArchiveSessionID+".source.gz")); err != nil {
		t.Fatal(err)
	}
	if used, err := s.admissionStageUsage(); err != nil || used <= deletionControlAllowance {
		t.Fatal("source lost its quota charge", used, err)
	}
	restarted, err := Open(s.home)
	if err != nil {
		t.Fatal(err)
	}
	if owed, err := restarted.HasDurableSessionEvidence(reg.ArchiveSessionID); err != nil || !owed {
		t.Fatal("durable obligation not detected by direct lookup", owed, err)
	}
	ids, err := restarted.OrphanedSessions(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != reg.ArchiveSessionID {
		t.Fatalf("durable source-only stage is invisible to orphan recovery: %v", ids)
	}
}

func TestAdmissionStageOrphansUseExactSafeSourceNames(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.Home(), admissionStageDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// Invalid bodies must not be decoded to inventory durable obligations.
	names := []string{"owner.source.gz", "owner.json", "owner.with.dots.source.gz", "owner.source.source.gz", ".source.gz", "..source.gz", "...source.gz", "owner.source.gz.tmp", "owner.source.gz.corrupt", "other.source.gzip", "unrelated.gz"}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not a source or manifest"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.OrphanedSessions(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"owner", "owner.source", "owner.with.dots"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("orphan IDs = %v, want %v", got, want)
	}
	got, err = s.OrphanedSessions(map[string]bool{"owner": true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want[1:]) {
		t.Fatalf("kept owner changed unrelated inventory: %v", got)
	}
	// Stage suffix recognition must not spread to deletion-control directories.
	controls := filepath.Join(s.Home(), "session-deletions")
	if err := os.MkdirAll(controls, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(controls, "unrelated.source.gz"), []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	ids, err := s.deletionOrphanStems("session-deletions")
	if err != nil || len(ids) != 0 {
		t.Fatal(ids, err)
	}
	regs, err := s.LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatal("inventory invented registrations", regs, err)
	}
	for _, name := range names {
		if raw, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(raw) != "not a source or manifest" {
			t.Fatal("inventory changed evidence", name, err)
		}
	}
}
