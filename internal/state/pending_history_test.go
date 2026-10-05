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
