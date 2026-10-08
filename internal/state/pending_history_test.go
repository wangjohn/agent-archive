package state

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestUnknownPendingHistoryRemainsUntouched(t *testing.T) {
	home := t.TempDir()
	s, err := openTestStore(home)
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
	s, err := openTestStore(t.TempDir())
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
	if err := s.RemovePending("session"); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatalf("source-only cleanup: %v", err)
	}
}

func TestStageCleanupFailureLeavesJournalAndRetries(t *testing.T) {
	s, err := openTestStore(t.TempDir())
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

func TestAbandonedStageSweepRequiresDescriptor(t *testing.T) {
	s, err := openTestStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.home, "sessions", "session", "pending-sources")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".pending-123456")
	if err = os.WriteFile(path, []byte("synthetic interrupted stage"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.SweepPendingSources("session"); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatalf("sweep: %v", err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("orphan evidence lost", err)
	}
}
