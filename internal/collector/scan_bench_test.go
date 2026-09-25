package collector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// largeTranscript is a codex transcript of n assistant messages of about 2 KiB
// each, the shape of a long working session.
func largeTranscript(n int) string {
	var b strings.Builder
	b.WriteString(`{"type":"turn_context","model":"gpt-test"}` + "\n")
	text := strings.Repeat("A long assistant message about the change in progress. ", 36)
	for i := range n {
		fmt.Fprintf(&b, `{"type":"response_item","id":"m%d","payload":{"type":"message","role":"assistant","content":%q}}`+"\n", i, text)
	}
	return b.String()
}

// benchmarkScan measures one collector pass over one large, already
// published session. prepare runs (untimed) before each pass to give it
// work: a hook request for an unchanged transcript, or a new record.
func benchmarkScan(b *testing.B, records int, prepare func(b *testing.B, local *state.Store, path string, i int)) {
	b.Helper()
	dir := b.TempDir()
	path := filepath.Join(dir, "codex.jsonl")
	if err := os.WriteFile(path, []byte(largeTranscript(records)), 0o600); err != nil {
		b.Fatal(err)
	}
	local, err := state.Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	reg := registration(&testing.T{}, path)
	if err := local.SaveRegistration(reg); err != nil {
		b.Fatal(err)
	}
	store := storage.NewMemoryStore()
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	opts := Options{MachineID: "m", Now: func() time.Time { return now }}
	if result, err := Run(context.Background(), local, store, opts); err != nil || len(result.Published) != 1 {
		b.Fatalf("%#v %v", result, err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		b.StopTimer()
		now = now.Add(time.Hour)
		prepare(b, local, path, i)
		b.StartTimer()
		result, err := Run(context.Background(), local, store, opts)
		if err != nil || len(result.Errors) != 0 {
			b.Fatalf("%#v %v", result, err)
		}
	}
}

// A hook request on a large session whose transcript has not changed: the
// pass reads, filters, and compares everything, and publishes nothing.
func BenchmarkScanLargeUnchangedSession(b *testing.B) {
	benchmarkScan(b, 2000, func(b *testing.B, local *state.Store, _ string, i int) {
		b.Helper()
		if err := local.SaveRequest("session-1", "stop", time.Date(2026, 2, 1, 0, 0, i, 0, time.UTC)); err != nil {
			b.Fatal(err)
		}
	})
}

// A large session that grew by one record since its last publication: the
// pass reads, compares, and publishes it again.
func BenchmarkScanLargeGrowingSession(b *testing.B) {
	benchmarkScan(b, 2000, func(b *testing.B, local *state.Store, path string, i int) {
		b.Helper()
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := fmt.Fprintf(f, `{"type":"response_item","id":"n%d","payload":{"type":"message","role":"assistant","content":"more"}}`+"\n", i); err != nil {
			b.Fatal(err)
		}
		if err := f.Close(); err != nil {
			b.Fatal(err)
		}
		if err := local.SaveRequest("session-1", "stop", time.Date(2026, 2, 1, 0, 0, i, 0, time.UTC)); err != nil {
			b.Fatal(err)
		}
	})
}
