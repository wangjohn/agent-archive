package discovery

import (
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/state"

	"github.com/wangjohn/agent-archive/internal/testutil/recoverytest"

	"context"
	"fmt"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Registration inventory size is separate from source history size. Unknown
// discovery identities require a census, including already archived sessions.
func BenchmarkUnknownDiscoveryWithRegistrationInventory(b *testing.B) {
	for _, count := range []int{1000, 10000, 100000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			store, cfg, at, root := fixture(b)
			project := cfg.Archive.Projects[0].Root
			for n := range count {
				reg := archive.SessionRegistration{ArchiveSessionID: fmt.Sprintf("archived-%d", n), NativeSessionID: fmt.Sprintf("native-history-%d", n), ProjectRoot: project, ProjectID: archive.ProjectID(project), Harness: archive.Harness{Name: "codex"}, SessionStartedAt: at.Add(time.Minute), AdmittedAt: at.Add(time.Minute), RegisteredAt: at.Add(time.Minute), Origin: archive.SessionOriginHook, DestinationID: cfg.DestinationID()}
				if err := store.SaveRegistration(reg); err != nil {
					b.Fatal(err)
				}
			}
			if err := recoverytest.Exhaust(context.Background(), store, state.SessionIndexRecoverySlice, false); err != nil {
				b.Fatal(err)
			}
			options := Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}
			var before, after syscall.Rusage
			_ = syscall.Getrusage(syscall.RUSAGE_SELF, &before)
			passes := 0
			b.ResetTimer()
			for n := range b.N {
				writeRollout(b, root, project, at.Add(time.Minute), count+n+1, "sessions")
				for {
					h, err := runScheduledSynthetic(context.Background(), store, cfg, options)
					if err != nil {
						b.Fatal(err)
					}
					passes++
					if h.Registered == 1 {
						break
					}
					if passes > 2*(n+1)+10 {
						b.Fatal("unknown identity did not progress")
					}
				}
			}
			b.StopTimer()
			_ = syscall.Getrusage(syscall.RUSAGE_SELF, &after)
			cpu := time.Duration((after.Utime.Sec-before.Utime.Sec)*1e9 + int64(after.Utime.Usec-before.Utime.Usec)*1e3)
			peakRSS := after.Maxrss
			if runtime.GOOS == "darwin" {
				peakRSS /= 1024
			}
			b.ReportMetric(float64(count), "authoritative-registrations")
			b.ReportMetric(float64(passes)/float64(b.N), "fresh-delay-passes")
			b.ReportMetric(float64(cpu.Milliseconds())/float64(b.N), "usercpu-ms/task")
			b.ReportMetric(float64(peakRSS), "process-maxrss-KiB")
		})
	}
}
