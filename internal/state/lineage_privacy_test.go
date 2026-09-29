package state

import (
	"testing"
	"time"
)

func TestRecordSupersededPreservesPrivacyMarkerWhenKeyReused(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	if err := store.RecordSupersededWithPrivacy("session-1", "source-a", first, true); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSupersededWithPrivacy("session-1", "source-b", first.Add(time.Minute), false); err != nil {
		t.Fatal(err)
	}
	later := first.Add(2 * time.Minute)
	if err := store.RecordSupersededWithPrivacy("session-1", "source-a", later, false); err != nil {
		t.Fatal(err)
	}
	ledger, err := store.LoadSuperseded("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger) != 2 || ledger[0].Key != "source-b" || ledger[1].Key != "source-a" || !ledger[1].PrivacySensitive || !ledger[1].SupersededAt.Equal(later) {
		t.Fatalf("reused source lost privacy marker or order: %#v", ledger)
	}
}
