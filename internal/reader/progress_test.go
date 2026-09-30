package reader

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// Progress is told how far a listing is: once per sidecar, with the total,
// counting up to it, and it is safe to call from the reading goroutines.
func TestListReportsProgressOncePerSidecar(t *testing.T) {
	store := newCountingStore()
	for i := range 25 {
		putSession(t, store, "codex", fmt.Sprintf("s%02d", i), baseTime.Add(time.Duration(i)*time.Minute))
	}
	store.delay = time.Millisecond
	var mu sync.Mutex
	var seen []int
	totals := map[int]bool{}
	progress := func(done, total int) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, done)
		totals[total] = true
	}
	if _, err := ListMetadataWithOptions(context.Background(), store, "sessions", Filter{}, ListOptions{Progress: progress}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(seen)
	if len(seen) != 25 || seen[0] != 1 || seen[24] != 25 || len(totals) != 1 || !totals[25] {
		t.Fatalf("progress calls = %v of totals %v, want 1..25 of 25", seen, totals)
	}
}
