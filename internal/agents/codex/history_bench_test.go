package codex

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

type historyIO struct {
	transcriptio.OS
	bytes    int64
	reads    int64
	opens    int64
	closes   int64
	closeErr error
}

type historyCountedFile struct {
	transcriptio.File
	owner *historyIO
}

func (f historyCountedFile) ReadAt(p []byte, off int64) (int, error) {
	n, e := f.File.ReadAt(p, off)
	f.owner.reads++
	f.owner.bytes += int64(n)
	return n, e
}

func (f historyCountedFile) Close() error {
	f.owner.closes++
	return errors.Join(f.File.Close(), f.owner.closeErr)
}

func (f *historyIO) OpenRegular(path string) (transcriptio.File, error) {
	v, e := f.OS.OpenRegular(path)
	if e != nil {
		return nil, e
	}
	f.opens++
	return historyCountedFile{v, f}, nil
}

func (f historyCountedFile) Stat() (fs.FileInfo, error) { return f.File.Stat() }

func BenchmarkRelatedHistoryRecords(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) { benchmarkHistory(b, n, 1) })
	}
	b.Run("shared_100000_four_children", func(b *testing.B) { benchmarkHistory(b, 100000, 4) })
}

// A benchmark operation assembles self-contained retained bundles sequentially.
// Raw file bytes are charged once per pass; safe records/maps remain outside that budget.
func benchmarkHistory(b *testing.B, n, children int) {
	b.Helper()
	dir := b.TempDir()
	texts := make([]string, n)
	for i := range texts {
		texts[i] = "synthetic prompt"
	}
	base, raw := historyFile(b, dir, threadA, threadA, 0, nil, texts...)
	baseBytes := len(raw)
	var leaves []agentapi.SourceRef
	for i := range children {
		id := fmt.Sprintf("%08x-2222-4222-8222-222222222222", i+1)
		leaf, _ := historyFile(b, dir, id, id, uint64(n+1), map[string]any{"parent_thread_id": threadA, "history_base": codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: uint64(n + 1), EndByteOffset: uint64(baseBytes)}}, "own child prompt")
		leaves = append(leaves, leaf)
	}
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Revision: "one"}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
	counted := &historyIO{}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	var peak atomic.Uint64
	peak.Store(before.HeapAlloc)
	sample := func() {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		for old := peak.Load(); m.HeapAlloc > old; old = peak.Load() {
			if peak.CompareAndSwap(old, m.HeapAlloc) {
				break
			}
		}
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sample()
			case <-stop:
				return
			}
		}
	}()
	defer func() { close(stop); <-done }()
	var charged, retained int64
	var cacheHits int64
	var filteredRecords int64
	b.ReportAllocs()
	b.SetBytes(int64(baseBytes * children))
	b.ResetTimer()
	for range b.N {
		p, e := (SourceProvider{}).OpenPass(context.Background(), agentapi.SourceEnvironment{Files: counted, CodexRollouts: lookup})
		if e != nil {
			b.Fatal(e)
		}
		for _, leaf := range leaves {
			lookup.thread.Current = &leaf
			s, e := p.Read(context.Background(), leaf, agentapi.ReadLimits{})
			if e != nil {
				b.Fatal(e)
			}
			charged = max(charged, p.(*relatedSourcePass).bytes)
			filtered, e := (Filter{}).Filter(context.Background(), s.Input(), agentapi.FilterContext{})
			if e != nil {
				b.Fatal(e)
			}
			retained = max(retained, int64(filtered.Boundary.RetainedBytes))
			filteredRecords += int64(len(filtered.Records))
			at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			reg := archive.SessionRegistration{ArchiveSessionID: "synthetic", NativeSessionID: filtered.History.ThreadID, ProjectID: "project", ProjectRoot: "/synthetic", Harness: archive.Harness{Name: "codex"}, TranscriptPath: leaf.Path, SessionStartedAt: at, RegisteredAt: at}
			bundle, e := archive.NewSourceBundle(reg, Filter{}, filtered, at, nil)
			if e != nil {
				b.Fatal(e)
			}
			sample()
			runtime.KeepAlive(bundle)
			runtime.KeepAlive(filtered)
			if e := s.Close(); e != nil {
				b.Fatal(e)
			}
		}
		cacheHits += p.(*relatedSourcePass).cacheHits
		if e := p.Close(); e != nil {
			b.Fatal(e)
		}
	}
	b.StopTimer()
	sample()
	if counted.opens != counted.closes {
		b.Fatalf("leaked descriptors %d/%d", counted.opens, counted.closes)
	}
	b.ReportMetric(float64(counted.bytes)/float64(b.N), "native_bytes/op")
	b.ReportMetric(float64(counted.reads)/float64(b.N), "reads/op")
	b.ReportMetric(float64(counted.opens)/float64(b.N), "opens/op")
	b.ReportMetric(float64(charged), "charged_peak_bytes")
	b.ReportMetric(float64(retained), "retained_encoded_bytes")
	b.ReportMetric(float64(peak.Load()-before.HeapAlloc), "heap_peak_delta_bytes")
	b.ReportMetric(float64(cacheHits)/float64(b.N), "prefix_hits/op")
	b.ReportMetric(float64(filteredRecords)/float64(b.N), "filtered_records/op")
	b.ReportMetric(0, "writes/op")
}

func TestSharedHistoryPassReusesBaseDescriptor(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, raw := historyFile(t, dir, threadA, threadA, 0, nil, "shared")
	counted := &historyIO{}
	lookup := &historyLookup{rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
	p, e := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{Files: counted, CodexRollouts: lookup})
	if e != nil {
		t.Fatal(e)
	}
	for i := range 4 {
		id := fmt.Sprintf("%08x-2222-4222-8222-222222222222", i+1)
		leaf, _ := historyFile(t, dir, id, id, 2, map[string]any{"parent_thread_id": threadA, "history_base": codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}}, "own")
		lookup.thread.Current = &leaf
		historyRead(t, p, leaf)
	}
	if counted.opens != 5 {
		t.Fatalf("base reopened per child: %d", counted.opens)
	}
	if e := p.Close(); e != nil {
		t.Fatal(e)
	}
	if counted.closes != counted.opens {
		t.Fatal("descriptor leak")
	}
}
