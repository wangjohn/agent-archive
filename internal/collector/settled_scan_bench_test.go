package collector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// BenchmarkScanSettledRegistrations measures the growing local-state inventory,
// independently of transcript size. All sources are synthetic and already settled.
// Counters are process-wide, so no parallel benchmark shares this measurement.
func BenchmarkScanSettledRegistrations(b *testing.B) {
	for _, sessions := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("sessions-%d", sessions), func(b *testing.B) {
			local, err := state.Open(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			path := filepath.Join(b.TempDir(), "synthetic.jsonl")
			if err := os.WriteFile(path, []byte("{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":\"synthetic\"}}\n"), 0600); err != nil {
				b.Fatal(err)
			}
			now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
			for i := range sessions {
				reg := archive.SessionRegistration{
					ArchiveSessionID: fmt.Sprintf("session-%05d", i), NativeSessionID: fmt.Sprintf("native-%05d", i),
					ProjectID: "synthetic", ProjectRoot: "/synthetic", Harness: archive.Harness{Name: "codex"},
					TranscriptPath: path, SessionStartedAt: now, RegisteredAt: now,
				}
				if err := local.SaveRegistration(reg); err != nil {
					b.Fatal(err)
				}
			}
			remote := &settledReadStore{MemoryStore: storagetest.NewMemoryStore()}
			opts := Options{Sources: testSources, MachineID: "synthetic", Now: func() time.Time { return now }}
			result, err := Run(context.Background(), local, remote, opts)
			if err != nil || len(result.Errors) != 0 || len(result.Published) != sessions {
				b.Fatalf("settle: %#v %v", result, err)
			}
			filters, loads, reads := transcriptFilters.Load(), state.PublishedStateLoads(), remote.reads
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				result, err := Run(context.Background(), local, remote, opts)
				if err != nil || len(result.Errors) != 0 || len(result.Skipped) != sessions || len(result.Published) != 0 {
					b.Fatalf("settled: %#v %v", result, err)
				}
			}
			b.StopTimer()
			if got := transcriptFilters.Load() - filters; got != 0 {
				b.Fatalf("settled pass filtered %d sources", got)
			}
			if got := state.PublishedStateLoads() - loads; got != 0 {
				b.Fatalf("settled pass decoded %d published bundles", got)
			}
			if got := remote.reads - reads; got != 0 {
				b.Fatalf("settled pass fetched %d remote objects", got)
			}
			b.ReportMetric(0, "remote-reads/op")
			b.ReportMetric(0, "filters/op")
			b.ReportMetric(0, "bundle-decodes/op")
		})
	}
}

// settledReadStore counts remote reads without changing the fake store semantics.
type settledReadStore struct {
	*storagetest.MemoryStore
	reads int
}

func (s *settledReadStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.reads++
	return s.MemoryStore.Get(ctx, key)
}

// A changed file must not cause its unchanged neighbours to be filtered.
// Sequential: transcriptFilters and PublishedStateLoads are process counters.
func TestOneChangedFileAmidSettledSessionsFiltersOnce(t *testing.T) {
	local := newTestStore(t)
	dir := t.TempDir()
	content := `{"type":"response_item","payload":{"type":"message","role":"assistant","content":"synthetic"}}` + "\n"
	ordinary := writeTranscript(t, dir, "settled.jsonl", content)
	changed := writeTranscript(t, dir, "changed.jsonl", content)
	for i := range 32 {
		path := ordinary
		if i == 0 {
			path = changed
		}
		reg := registration(t, path)
		reg.ArchiveSessionID = fmt.Sprintf("session-%d", i)
		reg.NativeSessionID = fmt.Sprintf("native-%d", i)
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
	}
	remote := &settledReadStore{MemoryStore: storagetest.NewMemoryStore()}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	opts := Options{Sources: testSources, MachineID: "synthetic", Now: func() time.Time { return now }}
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 32 {
		t.Fatalf("settle: %#v %v", result, err)
	}
	if err := os.WriteFile(changed, []byte(content+`{"type":"response_item","payload":{"type":"message","role":"assistant","content":"new"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err := local.SaveRequest("session-0", "stop", now); err != nil {
		t.Fatal(err)
	}
	filters, reads := transcriptFilters.Load(), remote.reads
	result, err = Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 || len(result.Skipped) != 31 {
		t.Fatalf("changed: %#v %v", result, err)
	}
	if got := transcriptFilters.Load() - filters; got != 1 {
		t.Fatalf("filters=%d, want 1", got)
	}
	if got := remote.reads - reads; got != 0 {
		t.Fatalf("remote reads=%d, want 0", got)
	}
}
