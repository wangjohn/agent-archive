package discovery

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Same 10k old-source fixture and adapter, with or without requested coverage.
// The baseline uses the pre-coverage catalog path; index scheduling is absent
// in both. Only the requested implementation runs the additional digest epoch.
func BenchmarkCatalogCoverageComparison(b *testing.B) {
	for _, shared := range []bool{false, true} {
		name := "baseline"
		if shared {
			name = "requested"
		}
		b.Run(name, func(b *testing.B) {
			store, cfg, at, root := fixture(b)
			wanted := ""
			for n := range 10000 {
				wanted = writeRollout(b, root, cfg.Archive.Projects[0].Root, at.Add(-time.Hour), n+1, "sessions")
			}
			adapter := &measuredCoverageAdapter{SourceAdapter: codexAdapter{supported: syntheticSupport}}
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			passes, writes := 0, 0
			bytes := int64(0)
			started := time.Now()
			b.ResetTimer()
			for {
				var l *CodexRolloutLookup
				var err error
				if shared {
					l, err = NewCodexRolloutLookup(b.Context(), store, []string{root})
					if err != nil {
						b.Fatal(err)
					}
					l.coverage.request(wanted)
				}
				h, err := runWithAdapters(context.Background(), store, cfg, Options{Now: func() time.Time { return at }, Rollouts: l}, []SourceAdapter{adapter})
				if err != nil {
					b.Fatal(err)
				}
				info, err := os.Stat(filepath.Join(store.Home(), "discovery-catalog.json"))
				if err != nil {
					b.Fatal(err)
				}
				bytes += info.Size()
				writes++
				passes++
				done := !h.Pending
				if shared {
					done = l.coverage.Phase == coverageComplete && l.coverage.Requests[wanted].CompleteEpoch == l.coverage.Epoch
					dirty := l.coverageDirty
					if err := l.Close(); err != nil {
						b.Fatal(err)
					}
					if dirty {
						info, err := os.Stat(filepath.Join(store.Home(), "discovery-catalog.json"))
						if err != nil {
							b.Fatal(err)
						}
						bytes += info.Size()
						writes++
					}
				}
				if done {
					break
				}
				if passes > 200 {
					b.Fatal("no convergence")
				}
			}
			b.StopTimer()
			runtime.ReadMemStats(&after)
			b.ReportMetric(float64(passes), "passes")
			b.ReportMetric(float64(writes), "catalog-writes")
			b.ReportMetric(float64(bytes), "catalog-written-B")
			b.ReportMetric(float64(time.Since(started).Milliseconds()), "convergence-ms")
			b.ReportMetric(float64(after.TotalAlloc-before.TotalAlloc), "allocated-B")
			b.ReportMetric(float64(adapter.headers), "header-probes")
			b.ReportMetric(float64(adapter.entries), "entry-fingerprints")
		})
	}
}
