package collector

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// Sessions with a pending request are processed first, oldest start first,
// then the rest in archive ID order. Progress reports each one; Stop ends
// the pass before the next session, leaving the rest pending.
func TestRunOrderProgressAndStop(t *testing.T) {
	dir := t.TempDir()
	local := newTestStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// IDs sort a..e; starts run the other way for the requested ones.
	starts := map[string]int{"a": 3, "b": 1, "c": 2, "d": 0, "e": 5}
	requested := map[string]bool{"a": true, "b": true, "c": true}
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		reg := registration(t, writeTranscript(t, dir, id+".jsonl", codexTranscript))
		reg.ArchiveSessionID, reg.NativeSessionID = id, "native-"+id
		reg.SessionStartedAt = base.Add(-time.Duration(starts[id]) * 24 * time.Hour)
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
		if requested[id] {
			if err := local.SaveRequest(id, "backfill", base); err != nil {
				t.Fatal(err)
			}
		}
	}
	now := base.Add(time.Hour)
	var order []string
	stopAfter := 0
	opts := Options{MachineID: "m", Now: func() time.Time { return now },
		Progress: func(p Progress) {
			order = append(order, fmt.Sprintf("%s:%t", p.ArchiveSessionID, p.Published))
		},
		Stop: func() bool { return stopAfter > 0 && len(order) >= stopAfter },
	}

	stopAfter = 2
	if _, err := Run(context.Background(), local, storagetest.NewMemoryStore(), opts); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a:true", "c:true"}; !slices.Equal(order, want) {
		t.Fatalf("stopped pass processed %v, want %v", order, want)
	}
	status, _ := local.LoadStatus()
	if status.PendingCount != 1 {
		t.Fatalf("pending after the stop: %d, want b", status.PendingCount)
	}
	if _, found, _ := local.LoadRequest("b"); !found {
		t.Fatal("b lost its request")
	}

	order, stopAfter = nil, 0
	local2 := newTestStore(t)
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		reg, _, _ := local.LoadRegistration(id)
		if err := local2.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
		if requested[id] {
			if err := local2.SaveRequest(id, "backfill", base); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := Run(context.Background(), local2, storagetest.NewMemoryStore(), opts); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a:true", "c:true", "b:true", "d:true", "e:true"}; !slices.Equal(order, want) {
		t.Fatalf("order %v, want %v", order, want)
	}
}
