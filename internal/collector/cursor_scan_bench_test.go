package collector

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// BenchmarkScanChangedCursorChats reuses one snapshot for 100 changed chats.
// Native writes and initial publication are outside the measured pass.
// Sequential: the synthetic database owns the process snapshot-directory seam.
func BenchmarkScanChangedCursorChats(b *testing.B) {
	db := newCursorDB(b, true)
	local, err := state.Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	for i := range 100 {
		id := fmt.Sprintf("chat-%03d", i)
		db.chat(id, 1, "first")
		if err := local.SaveRegistration(cursorRegistration("session-"+id, id)); err != nil {
			b.Fatal(err)
		}
	}
	copies := 0
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	opts := Options{Sources: testSources, MachineID: "synthetic", CursorDatabase: db.path, afterCursorPass: func(n int) { copies = n }, Now: func() time.Time { return now }}
	remote := storagetest.NewMemoryStore()
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 100 || copies != 1 {
		b.Fatalf("settle: published=%d copies=%d errors=%v %v", len(result.Published), copies, result.Errors, err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		b.StopTimer()
		ids := []string{"first"}
		for j := range i + 1 {
			ids = append(ids, fmt.Sprintf("next-%d", j))
		}
		now = now.Add(time.Hour)
		for j := range 100 {
			id := fmt.Sprintf("chat-%03d", j)
			db.chat(id, int64(i+2), ids...)
			if err := local.SaveRequest("session-"+id, "stop", now); err != nil {
				b.Fatal(err)
			}
		}
		b.StartTimer()
		result, err := Run(context.Background(), local, remote, opts)
		if err != nil || len(result.Errors) != 0 || len(result.Published) != 100 || copies != 1 {
			b.Fatalf("changed: published=%d copies=%d errors=%v %v", len(result.Published), copies, result.Errors, err)
		}
	}
	b.StopTimer()
	b.ReportMetric(1, "snapshot-copies/op")
}
