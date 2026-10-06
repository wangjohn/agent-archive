package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Initial admission benchmark includes the real native registry, source snapshot,
// current permission checks, durable registration and actual source/metadata publication.
func BenchmarkNativeChildInitialAdmission(b *testing.B) {
	for _, count := range []int{1000, 10000, 100000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			for iteration := range b.N {
				b.StopTimer()
				store, cfg, at, home := fixture(b)
				id := fmt.Sprintf("00000000-0000-0000-0000-%012d", iteration+100)
				parent := "00000000-0000-0000-0000-000000000001"
				meta := map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "session_id": parent, "parent_thread_id": parent, "cwd": cfg.Archive.Projects[0].Root, "timestamp": "2026-10-01T12:01:00Z", "cli_version": "dev", "originator": "new-client", "source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent, "depth": 1}}}, "subagent_history_start_ordinal": count + 1}}
				header, _ := json.Marshal(meta)
				var raw strings.Builder
				raw.Write(header)
				raw.WriteByte('\n')
				for range count {
					raw.WriteString(`{"type":"turn_context","payload":{"model":"synthetic inherited model","synthetic_padding":"`)
					raw.WriteString(strings.Repeat("x", 128))
					raw.WriteString(`"}}`)
					raw.WriteByte('\n')
				}
				raw.WriteString(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"` + id + `","started_at":"2026-10-01T12:01:00Z"}}`)
				raw.WriteByte('\n')
				raw.WriteString(`{"type":"response_item","timestamp":"2026-10-01T12:01:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic own prompt"}]}}`)
				raw.WriteByte('\n')
				path := filepath.Join(home, "sessions", "rollout-"+id+".jsonl")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					b.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(raw.String()), 0600); err != nil {
					b.Fatal(err)
				}
				nativeBytes := raw.Len()
				raw.Reset()
				resources := &childBenchmarkResources{SourcesLookup: builtin.NewBuiltins()}
				options := Options{Sources: resources, Now: func() time.Time { return at.Add(2 * time.Minute) }}
				decodes := state.PublishedStateLoads()
				runtime.GC()
				var memoryBefore runtime.MemStats
				runtime.ReadMemStats(&memoryBefore)
				stopMemory := childBenchmarkMemory()
				b.StartTimer()
				health, err := runWithCensus(context.Background(), store, cfg, options, registeredAdapters())
				if err != nil || health.Registered != 1 {
					b.Fatalf("initial admission count=%d health=%+v err=%v", count, health, err)
				}
				cloud := storagetest.NewMemoryStore()
				published, publishErr := collector.Run(context.Background(), store, cloud, collector.Options{Sources: resources, Parsers: builtin.NewBuiltins(), MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: options.Now})
				b.StopTimer()
				peakHeap := stopMemory()
				var memoryAfter runtime.MemStats
				runtime.ReadMemStats(&memoryAfter)
				b.ReportMetric(float64(peakHeap), "sampled-live-heap-bytes")
				b.ReportMetric(float64(memoryAfter.TotalAlloc-memoryBefore.TotalAlloc), "operation-allocated-bytes")
				var usage syscall.Rusage
				if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) == nil {
					rss := usage.Maxrss
					if runtime.GOOS == "linux" {
						rss *= 1024
					}
					b.ReportMetric(float64(rss), "process-maxrss-bytes")
				}
				used, peak := resources.charged()
				b.ReportMetric(float64(peak), "publication-charged-peak-bytes")
				if used != 0 || peak > 128<<20 {
					b.Fatalf("resource ownership leak/refusal: used=%d peak=%d", used, peak)
				}
				if publishErr != nil || len(published.Published) != 1 || len(published.Errors) != 0 {
					b.Fatalf("publication count=%d result=%+v err=%v", count, published, publishErr)
				}
				b.ReportMetric(float64(nativeBytes), "native-file-bytes")
				b.ReportMetric(float64(health.NativeValidationBytes), "native-validation-bytes")
				b.ReportMetric(float64(health.NativeValidationReads), "native-read-calls")
				b.ReportMetric(float64(health.NativeValidationOpens), "native-opens")
				b.ReportMetric(float64(health.Probes), "header-probes")
				b.ReportMetric(float64(health.NativeValidationAttempts), "native-proof-attempts")
				b.ReportMetric(float64(state.PublishedStateLoads()-decodes), "full-published-decodes")
				before := childBenchmarkState(b, store.Home())
				settledLoads := state.PublishedStateLoads()
				reopened, err := state.Open(store.Home())
				if err != nil {
					b.Fatal(err)
				}
				settledStart := time.Now()
				settled, err := collector.Run(context.Background(), reopened, cloud, collector.Options{Sources: resources, Parsers: builtin.NewBuiltins(), MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: options.Now})
				b.ReportMetric(float64(time.Since(settledStart).Nanoseconds()), "settled-ns")
				fullLoads := state.PublishedStateLoads() - settledLoads
				b.ReportMetric(float64(fullLoads), "settled-full-decodes")
				if err != nil || len(settled.Errors) != 0 || len(settled.Skipped) != 1 || fullLoads != 0 || !reflect.DeepEqual(before, childBenchmarkState(b, store.Home())) {
					b.Fatalf("settled child changed or decoded retained state: %+v %v loads=%d", settled, err, fullLoads)
				}
				b.ReportMetric(0, "settled-session-writes")

			}
		})
	}
}

func childBenchmarkState(b *testing.B, home string) map[string]int64 {
	b.Helper()
	out := map[string]int64{}
	for _, dir := range []string{"registrations", "published", "scan-signatures", "pending-scans", "pending", "requests"} {
		err := filepath.WalkDir(filepath.Join(home, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			out[path] = info.ModTime().UnixNano()
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			b.Fatal(err)
		}
	}
	return out
}

// This observer records the production pass ledger without replacing provider logic.
type childBenchmarkResources struct {
	agentapi.SourcesLookup
	budgets []*agentapi.NativeReadBudget
}

func (s *childBenchmarkResources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	provider, filter, ok := s.SourcesLookup.LookupSources(name)
	return childBenchmarkProvider{SourceProvider: provider, owner: s}, filter, ok
}

type childBenchmarkProvider struct {
	agentapi.SourceProvider
	owner *childBenchmarkResources
}

func (p childBenchmarkProvider) OpenPass(ctx context.Context, e agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	if e.ReadBudget != nil {
		p.owner.budgets = append(p.owner.budgets, e.ReadBudget)
	}
	return p.SourceProvider.OpenPass(ctx, e)
}

func (s *childBenchmarkResources) charged() (used, peak int64) {
	seen := map[*agentapi.NativeReadBudget]bool{}
	for _, budget := range s.budgets {
		if !seen[budget] {
			seen[budget] = true
			u, p := budget.Charged()
			used += u
			peak = max(peak, p)
		}
	}
	return used, peak
}

func childBenchmarkMemory() func() uint64 {
	stop, done := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var peak uint64
	sample := func() {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		mu.Lock()
		peak = max(peak, m.HeapAlloc)
		mu.Unlock()
	}
	sample()
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sample()
			case <-stop:
				sample()
				return
			}
		}
	}()
	return func() uint64 { close(stop); <-done; mu.Lock(); defer mu.Unlock(); return peak }
}
