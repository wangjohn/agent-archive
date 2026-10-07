package state

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestHistoryGenerationRecoveryLeavesStateUnchanged(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	published, err := store.LoadPublishedState("protected")
	if err != nil {
		t.Fatal(err)
	}
	if err := published.Save(archive.SourceBundle{SchemaVersion: archive.HistorySourceSchemaVersion, History: &archive.SourceHistory{}}, time.Now(), CacheStatusPublished); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(store.publishedPath("protected"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginGenerationRecovery("protected", time.Now(), func(archive.SessionRegistration, string) (archive.SessionRegistration, PendingPublication, error) {
		t.Fatal("protected builder called")
		return archive.SessionRegistration{}, PendingPublication{}, nil
	}); !errors.Is(err, archive.ErrHistoryMutationPending) {
		t.Fatalf("history recovery: %v", err)
	}
	after, err := os.ReadFile(store.publishedPath("protected"))
	if err != nil || string(after) != string(before) {
		t.Fatal("protected state changed")
	}
	if err := validateGenerationPublication(archive.SessionRegistration{}, PendingPublication{Bundle: archive.SourceBundle{SchemaVersion: archive.HistorySourceSchemaVersion}}, nil, t.Context()); !errors.Is(err, archive.ErrHistoryMutationPending) {
		t.Fatalf("replay history: %v", err)
	}
}
