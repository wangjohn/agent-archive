package state

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func BenchmarkScheduledSessionIndexRecovery100k(b *testing.B) {
	for _, candidates := range []int{0, 100, 1000} {
		b.Run(fmt.Sprintf("candidates-%d", candidates), func(b *testing.B) { benchmarkScheduledRecovery100k(b, candidates) })
	}
}

func benchmarkScheduledRecovery100k(b *testing.B, candidateCount int) {
	b.Helper()
	s, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	for i := range 100000 {
		id := fmt.Sprintf("owner-%06d", i)
		key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: fmt.Sprintf("native-%06d", i)}
		data, err := json.Marshal(migrationRegistration(key, id))
		if err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.home, "registrations", id+".json"), data, 0600); err != nil {
			b.Fatal(err)
		}
	}
	for i := range candidateCount {
		candidate := SubagentCandidate{ArchiveSessionID: fmt.Sprintf("child-owner-%06d", i), NativeSessionID: fmt.Sprintf("child-native-%06d", i), ParentArchiveSessionID: "owner-000000", ParentNativeSessionID: "native-000000", ProjectID: "p", ProjectRoot: "/synthetic", Harness: archive.Harness{Name: "codex"}, AgentID: fmt.Sprintf("agent-%06d", i), TranscriptPath: "/synthetic/child.jsonl", ObservedAt: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)}
		data, err := json.Marshal(candidate)
		if err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(s.subagentCandidatePath(candidate.ArchiveSessionID), data, 0600); err != nil {
			b.Fatal(err)
		}
	}
	runtime.GC()
	b.ReportAllocs()
	var usageBefore, usageAfter syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &usageBefore)
	b.ResetTimer()
	var maxSlice time.Duration
	var slices int
	var peak uint64
	var peakRSS uint64
	for range b.N {
		if err := s.MarkSessionIndexRecoveryNeeded(); err != nil {
			b.Fatal(err)
		}
		done := make(chan struct{})
		var samples sync.WaitGroup
		samples.Add(1)
		go func() {
			defer samples.Done()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					var m runtime.MemStats
					runtime.ReadMemStats(&m)
					if m.HeapAlloc > peak {
						peak = m.HeapAlloc
					}
					if rss := sampleRecoveryRSS(); rss > peakRSS {
						peakRSS = rss
					}
				}
			}
		}()
		complete := false
		for attempts := 0; attempts < 20 && !complete; attempts++ {
			started := time.Now()
			complete, err = s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice)
			elapsed := time.Since(started)
			if elapsed > maxSlice {
				maxSlice = elapsed
			}
			slices++
			if err != nil {
				close(done)
				samples.Wait()
				b.Fatal(err)
			}
		}
		close(done)
		samples.Wait()
		if !complete {
			b.Fatal("recovery did not converge in twenty slices")
		}
	}
	b.StopTimer()
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &usageAfter)
	cpuUser := float64(usageAfter.Utime.Sec-usageBefore.Utime.Sec)*1000 + float64(usageAfter.Utime.Usec-usageBefore.Utime.Usec)/1000
	cpuSystem := float64(usageAfter.Stime.Sec-usageBefore.Stime.Sec)*1000 + float64(usageAfter.Stime.Usec-usageBefore.Stime.Usec)/1000
	maxRSS := usageAfter.Maxrss
	if runtime.GOOS == "darwin" {
		maxRSS /= 1024
	}
	b.ReportMetric(cpuUser/float64(b.N), "usercpu-ms/op")
	b.ReportMetric(cpuSystem/float64(b.N), "systemcpu-ms/op")
	b.ReportMetric(float64(maxRSS), "process-maxrss-KiB")
	b.ReportMetric(maxSlice.Seconds(), "max-slice-sec")
	b.ReportMetric(float64(slices)/float64(b.N), "slices/op")
	b.ReportMetric(float64(peak), "sampled-peak-heap-B")
	b.ReportMetric(float64(peakRSS), "sampled-peak-rss-B")
}

func sampleRecoveryRSS() uint64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "VmRSS:" {
			value, err := strconv.ParseUint(fields[1], 10, 64)
			if err == nil {
				return value * 1024
			}
		}
	}
	return 0
}
