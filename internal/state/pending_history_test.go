package state

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestUnknownPendingHistoryRemainsUntouched(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	path := s.pendingPath("session")
	raw := []byte(`{"history":{"version":99},"source_key":"synthetic"}`)
	if err := local.WriteBytes(path, raw); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ForCollectorPass().LoadPending("session"); err == nil {
		t.Fatal("future history was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(raw) {
		t.Fatalf("future history changed: %s %v", after, err)
	}
}

func TestStagedHistorySourceIsBoundedAndChecksumNamed(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("synthetic immutable revision")
	sum := sha256.Sum256(data)
	ref := archive.SourceReference{Key: "synthetic", SHA256: hex.EncodeToString(sum[:]), CompressedBytes: len(data)}
	stage, err := s.StagePendingSource("session", ref, data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadPendingSource("session", stage); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StagePendingSource("session", ref, []byte("different")); err == nil {
		t.Fatal("bad staged checksum accepted")
	}
	stage.Name = "../escape.gz"
	if _, err := s.ReadPendingSource("session", stage); err == nil {
		t.Fatal("stage escape accepted")
	}
	if err := s.RemovePending("session"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.home, "sessions", "session", "pending-sources")); !os.IsNotExist(err) {
		t.Fatalf("stages were not removed: %v", err)
	}
}

func TestStageCleanupFailureLeavesJournalAndRetries(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := "session"
	journal := s.pendingPath(id)
	if err := local.WriteBytes(journal, []byte(`{"synthetic":"cleanup obligation"}`)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.home, "sessions", id, "pending-sources")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	unsafe := filepath.Join(dir, "unsafe-entry")
	if err := os.WriteFile(unsafe, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.RemovePending(id); err == nil {
		t.Fatal("unsafe cleanup succeeded")
	}
	if _, err := os.Stat(journal); err != nil {
		t.Fatal("cleanup obligation lost", err)
	}
	if err := os.Remove(unsafe); err != nil {
		t.Fatal(err)
	}
	if err := s.RemovePending(id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestAbandonedStageSweepIsBoundedAndRefusesUnsafeTypes(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.home, "sessions", "session", "pending-sources")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for i := range pendingStageCleanupBatch + 1 {
		sum := sha256.Sum256([]byte{byte(i)})
		if err := os.WriteFile(filepath.Join(dir, hex.EncodeToString(sum[:])+".gz"), []byte("synthetic"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SweepPendingSources("session"); err == nil {
		t.Fatal("unbounded sweep")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("incorrect bounded remainder", len(entries), err)
	}
	if err := s.SweepPendingSources("session"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}
	if err := s.SweepPendingSources("session"); err == nil {
		t.Fatal("symlink stage directory accepted")
	}
}

func TestAbandonedStageTempSweepsUnderCollectorOwnership(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.home, "sessions", "session", "pending-sources", ".pending-123456")
	if err := local.WriteBytes(path, []byte("synthetic interrupted stage")); err != nil {
		t.Fatal(err)
	}
	if err := s.SweepPendingSources("session"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
