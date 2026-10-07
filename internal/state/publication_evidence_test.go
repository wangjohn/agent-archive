package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPublicationEvidencePhysicalFilesAndOrphansStayCharged(t *testing.T) {
	t.Parallel()
	s, _, _ := stageFixture(t)
	dir := filepath.Join(s.Home(), publicationEvidenceDir, "synthetic")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"journal.json", "orphan.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("retained"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	reads := 0
	s.onQuotaBodyRead = func(string) { reads++ }
	used, err := s.admissionStageUsage()
	if err != nil || used != deletionControlAllowance+publicationEvidenceControl+4*int64(len("retained")) || reads != 0 {
		t.Fatal(used, reads, err)
	}
	outside := t.TempDir()
	if err = os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.admissionStageUsage(); !errors.Is(err, ErrAdmissionStageRecovery) {
		t.Fatal("quota followed evidence escape", err)
	}
}
