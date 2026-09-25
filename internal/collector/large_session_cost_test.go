package collector

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// countingStore counts the bytes a pass moves to and from storage: egress
// (Get) is what a republish of a large bundle pays for.
type countingStore struct {
	*storagetest.MemoryStore
	getBytes atomic.Int64
	putBytes atomic.Int64
}

func (s *countingStore) Get(ctx context.Context, key string) ([]byte, error) {
	b, err := s.MemoryStore.Get(ctx, key)
	s.getBytes.Add(int64(len(b)))
	return b, err
}

func (s *countingStore) Put(ctx context.Context, key string, data []byte) error {
	s.putBytes.Add(int64(len(data)))
	return s.MemoryStore.Put(ctx, key, data)
}

func (s *countingStore) reset() {
	s.getBytes.Store(0)
	s.putBytes.Store(0)
}

// largePassCost is what one pass over one large session cost.
type largePassCost struct {
	elapsed    time.Duration
	allocated  uint64 // bytes allocated during the pass
	peakHeap   uint64 // highest live heap seen during the pass
	downloaded int64  // bytes read from storage
	uploaded   int64  // bytes written to storage
}

func (c largePassCost) String() string {
	return fmt.Sprintf("%s, allocated %d MB, peak heap %d MB, downloaded %d KB, uploaded %d KB",
		c.elapsed.Round(time.Millisecond), c.allocated>>20, c.peakHeap>>20, c.downloaded>>10, c.uploaded>>10)
}

// measureLargePass runs one pass and measures it: time, allocation, the
// live heap's peak (sampled), and storage traffic.
func measureLargePass(t *testing.T, local *state.Store, remote *countingStore, opts Options) (Result, largePassCost) {
	t.Helper()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	remote.reset()
	var peak atomic.Uint64
	stop := make(chan struct{})
	var sampling sync.WaitGroup
	sampling.Go(func() {
		sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			metrics.Read(sample)
			if v := sample[0].Value.Uint64(); v > peak.Load() {
				peak.Store(v)
			}
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	})
	start := time.Now()
	result, err := Run(context.Background(), local, remote, opts)
	elapsed := time.Since(start)
	close(stop)
	sampling.Wait()
	if err != nil {
		t.Fatal(err)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	return result, largePassCost{
		elapsed: elapsed, allocated: after.TotalAlloc - before.TotalAlloc, peakHeap: peak.Load(),
		downloaded: remote.getBytes.Load(), uploaded: remote.putBytes.Load(),
	}
}

// A long working session keeps growing, and every pass that sees it grow
// publishes it again. At the documented size (AGENT_ARCHIVE_PERF=1) this is
// a 30 MB transcript whose content all survives filtering; otherwise a small
// one, which still checks what each pass may download and read.
//
// A 2026-09 pre-release review measured about 10 s, 1.1 to 1.5 GB allocated,
// and a download of the whole compressed source (9 MB here) for each of the
// three passes, and a 32 MB published state file. Now a publication takes
// about 4 s and 0.6 to 0.8 GB, the refresh about 1.2 s without reading the
// transcript, and none of them downloads anything. The limits below leave
// room for a slower machine.
//
// Not parallel: it measures the process's allocation and reads a
// package-wide counter.
func TestLargeGrowingSessionPassesStayFast(t *testing.T) {
	records, timed := 200, false
	if os.Getenv(perfEnv) != "" {
		if raceEnabled {
			t.Skip("the wall-clock target is for a plain build, not the race detector")
		}
		records, timed = 15000, true
	}
	home := t.TempDir()
	local, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	path := writeTranscript(t, t.TempDir(), "codex.jsonl", variedTranscript(records))
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	remote := &countingStore{MemoryStore: storagetest.NewMemoryStore()}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	opts := Options{MachineID: "m", Now: func() time.Time { return now }}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	result, first := measureLargePass(t, local, remote, opts)
	if len(result.Published) != 1 || len(result.Errors) != 0 {
		t.Fatalf("first pass: %#v", result)
	}
	t.Logf("%d MB transcript, first publication: %s", info.Size()>>20, first)

	appendRecord(t, path, `{"type":"response_item","id":"more","payload":{"type":"message","role":"assistant","content":"more"}}`)
	now = now.Add(time.Hour)
	if err := local.SaveRequest("session-1", "stop", now); err != nil {
		t.Fatal(err)
	}
	result, grown := measureLargePass(t, local, remote, opts)
	if len(result.Published) != 1 || len(result.Errors) != 0 {
		t.Fatalf("growing pass: %#v", result)
	}
	stateInfo, err := os.Stat(filepath.Join(home, "published", "session-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("republication after one new record: %s; published state %d KB", grown, stateInfo.Size()>>10)

	now = now.Add(time.Hour)
	opts.ParserVersion = "perf-next-parser"
	filters := transcriptFilters.Load()
	result, refreshed := measureLargePass(t, local, remote, opts)
	if len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("parser refresh pass: %#v", result)
	}
	t.Logf("metadata refresh after a parser bump: %s", refreshed)
	if filters := transcriptFilters.Load() - filters; filters != 0 {
		t.Errorf("the metadata refresh read the unchanged transcript %d times", filters)
	}

	// A store that reports each object's checksum verifies a publication
	// without downloading it.
	for name, cost := range map[string]largePassCost{"first publication": first, "republication": grown, "metadata refresh": refreshed} {
		if cost.downloaded != 0 {
			t.Errorf("%s downloaded %d bytes", name, cost.downloaded)
		}
	}
	if !timed {
		return
	}
	for name, cost := range map[string]largePassCost{"first publication": first, "republication": grown} {
		if cost.elapsed > 7*time.Second || cost.allocated > 1<<30 {
			t.Errorf("%s: %s, want under 7 s and 1 GB", name, cost)
		}
	}
	if refreshed.elapsed > 3*time.Second {
		t.Errorf("metadata refresh: %s, want under 3 s", refreshed)
	}
}

// variedTranscript is a codex transcript of n assistant messages of about 2
// KiB each, like largeTranscript, but of words drawn at random from a
// working session's vocabulary, so it compresses about as well as real
// prose does rather than to almost nothing.
func variedTranscript(n int) string {
	words := strings.Fields(`the a to of and in is it that for on with this be as are was at by
		from or an not but can will if we you they have has had do does did so all
		one when which would there their what about out up into than then them
		these some could other more also only new just like time over after
		function return error value file test build change code line package
		import type struct string int bool map slice config path directory run
		call method field result check update read write open close parse
		output input request response server client handler context cancel
		loop index length count buffer bytes format print log debug trace
		commit branch merge diff review patch release version module go make
		fix bug issue case default switch range append copy delete sort
		compare equal hash sum limit size cache store load save state lock`)
	random := rand.New(rand.NewPCG(1, 2))
	var b strings.Builder
	b.WriteString(`{"type":"turn_context","model":"gpt-test"}` + "\n")
	var text strings.Builder
	for i := range n {
		text.Reset()
		for text.Len() < 2000 {
			text.WriteString(words[random.IntN(len(words))])
			text.WriteByte(' ')
		}
		fmt.Fprintf(&b, `{"type":"response_item","id":"m%d","payload":{"type":"message","role":"assistant","content":%q}}`+"\n", i, text.String())
	}
	return b.String()
}

func appendRecord(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
