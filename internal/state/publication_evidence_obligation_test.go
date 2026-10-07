package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPublicationEvidenceOrphanRemainsPendingWithoutBodyReads(t *testing.T) {
	s := newTestStore(t)
	id := "synthetic"
	dir := filepath.Join(s.Home(), publicationEvidenceDir, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "interrupted.tmp"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	s.onQuotaBodyRead = func(string) { t.Fatal("obligation inspection decoded body") }
	owed, err := s.HasPending(id)
	if err != nil || !owed {
		t.Fatal("orphan evidence disappeared", owed, err)
	}
	if err := os.Remove(filepath.Join(dir, "interrupted.tmp")); err != nil {
		t.Fatal(err)
	}
	owed, err = s.HasPending(id)
	if err != nil || owed {
		t.Fatal("empty directory invented obligation", owed, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "journal.json"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	owed, err = s.HasPending(id)
	if err != nil || !owed {
		t.Fatal("malformed evidence disappeared", owed, err)
	}
}
