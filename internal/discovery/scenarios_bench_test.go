package discovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

// These synthetic measurements are machinery evidence, never producer or
// release-machine acceptance. The bounded cache's misses and cold coverage
// remain visible rather than implying a universal warm-task latency promise.
func BenchmarkCodexCoverageAndWarmBurst(b *testing.B) {
	for _, count := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			benchmarkCoverageAndWarmBurst(b, count, false)
		})
	}
}

func BenchmarkNativeDateCoverageAndWarmBurst(b *testing.B) {
	for _, count := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) { benchmarkCoverageAndWarmBurst(b, count, true) })
	}
}

func benchmarkCoverageAndWarmBurst(b *testing.B, count int, dated bool) {
	store, cfg, at, root := fixture(b)
	project := cfg.Archive.Projects[0].Root
	historyFolder, freshFolder := "sessions", "sessions"
	historyAt := at.Add(-time.Hour)
	if dated {
		historyFolder = "sessions/2020/10/01"
		freshFolder = "sessions/2026/10/01"
		historyAt = at.AddDate(-6, 0, 0)
	}
	for i := 1; i <= count; i++ {
		writeRollout(b, root, project, historyAt, i, historyFolder)
	}
	started := time.Now()
	var before syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &before)
	passes, readBytes, probes := 0, int64(0), 0
	for {
		h, err := run(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport)
		if err != nil {
			b.Fatal(err)
		}
		passes++
		readBytes += h.Bytes
		probes += h.Probes
		if !h.Pending {
			break
		}
		if passes > count/64+100 {
			b.Fatal("coverage did not converge")
		}
	}
	coverageWall := time.Since(started)
	var after syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &after)
	cpu := time.Duration((after.Utime.Sec-before.Utime.Sec)*1e9 + int64(after.Utime.Usec-before.Utime.Usec)*1e3)
	// An incomplete file and one fresh task appear after completed coverage.
	incompleteID := writeRollout(b, root, project, at.Add(time.Minute), count+1, "archived_sessions")
	incomplete := filepath.Join(root, "archived_sessions", "rollout-2026-10-01T12-00-00-"+incompleteID+".jsonl")
	if err := os.WriteFile(incomplete, []byte("{\"type\":\"session_meta\""), 0600); err != nil {
		b.Fatal(err)
	}
	writeRollout(b, root, project, at.Add(time.Minute), count+2, freshFolder)
	delay := 0
	burstStart := time.Now()
	b.ResetTimer()
	warmProbes := 0
	for i := 0; i < b.N; i++ {
		h, err := run(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport)
		if err != nil {
			b.Fatal(err)
		}
		warmProbes += h.Probes
	}
	b.StopTimer()
	for {
		regs, _ := store.LoadRegistrations()
		if len(regs) > 0 {
			break
		}
		h, err := run(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport)
		if err != nil {
			b.Fatal(err)
		}
		_ = h
		delay++
		if delay > count/64+100 {
			b.Fatal("fresh task starved")
		}
	}
	b.ReportMetric(float64(passes), "cold-passes")
	b.ReportMetric(float64(probes), "coverage-headers")
	b.ReportMetric(float64(readBytes), "coverage-bytes")
	b.ReportMetric(float64(coverageWall.Milliseconds()), "coverage-ms")
	b.ReportMetric(float64(cpu.Milliseconds()), "coverage-usercpu-ms")
	b.ReportMetric(float64(warmProbes)/float64(b.N), "warm-headers/pass")
	b.ReportMetric(float64(delay+b.N), "fresh-delay-passes")
	b.ReportMetric(float64(time.Since(burstStart).Milliseconds()), "fresh-delay-ms")
}

func BenchmarkSyntheticAdmissionLock(b *testing.B) {
	store, cfg, at, root := fixture(b)
	project := cfg.Archive.Projects[0].Root
	const count = 1000
	headers := make([]sourcefacts.Header, count)
	locators := make([]string, count)
	for i := 0; i < count; i++ {
		id := writeRollout(b, root, project, at.Add(time.Minute), i+1, "sessions")
		locators[i] = filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+id+".jsonl")
		headers[i] = sourcefacts.ReadHeader(context.Background(), root, locators[i])
	}
	durations := make([]time.Duration, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		index := i % count
		header := headers[index]
		generation, _ := cfg.DiscoveryGeneration("codex", project, header.Started, at.Add(2*time.Minute))
		started := time.Now()
		if _, _, err := admit(store, header, root, locators[index], project, generation, at.Add(2*time.Minute)); err != nil {
			b.Fatal(err)
		}
		durations[i] = time.Since(started)
	}
	b.StopTimer()
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	b.ReportMetric(float64(durations[(len(durations)-1)*99/100].Microseconds())/1000, "admission-p99-ms")
}
