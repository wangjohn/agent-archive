package state

import (
	"fmt"
	"testing"
)

func TestListingRepairSlicesRotateAcrossFailedIntents(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 70 {
		if err = store.SaveListingRepair(fmt.Sprintf("s%03d", i), ListingRepair{MetadataKey: fmt.Sprintf("sessions/codex/s%03d/metadata.json", i), DestinationID: "destination"}); err != nil {
			t.Fatal(err)
		}
	}
	seen := make(map[string]bool)
	for range 3 {
		reopened, err := Open(store.Home())
		if err != nil {
			t.Fatal(err)
		}
		slice, err := reopened.ListingRepairs(32)
		if err != nil {
			t.Fatal(err)
		}
		if len(slice) != 32 {
			t.Fatalf("slice size=%d", len(slice))
		}
		for id := range slice {
			seen[id] = true
		}
	}
	if len(seen) != 70 {
		t.Fatalf("failed early intents starved later ones: saw %d", len(seen))
	}
}
