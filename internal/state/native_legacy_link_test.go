package state

import (
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"os"
	"testing"
	"time"
)

func TestLegacyCodexCompositeReservationIsEvidenceOnly(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parent := "00000000-0000-0000-0000-000000000001"
	child := "00000000-0000-0000-0000-000000000002"
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: parent + ":subagent:" + child}
	id, _, err := store.EnsureArchiveSessionID(key)
	if err != nil {
		t.Fatal(err)
	}
	got, proved, err := store.LegacyCodexCompositeReservation(parent, child)
	if err != nil || !proved || got != id {
		t.Fatalf("positive legacy reservation %s %v %v", got, proved, err)
	}
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	reg := archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: key.NativeID, Harness: archive.Harness{Name: "codex"}, ProjectID: "project", ProjectRoot: "/synthetic/project", TranscriptPath: "/synthetic/source.jsonl", SessionStartedAt: at, RegisteredAt: at}
	if err := store.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if _, proved, err := store.LegacyCodexCompositeReservation(parent, child); err != nil || proved {
		t.Fatal("admitted registration treated as a ghost", err)
	}
	// Losing derived evidence never authorizes deleting/omitting an unknown link.
	if err := os.Remove(qualifiedSessionIndexPath(store.Home(), key)); err != nil {
		t.Fatal(err)
	}
	if _, proved, err := store.LegacyCodexCompositeReservation(parent, child); err != nil || proved {
		t.Fatal("missing local state proved a ghost", err)
	}
}
