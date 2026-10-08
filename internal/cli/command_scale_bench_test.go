package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// BenchmarkCommandScale measures Run: config/startup, cache, discovery, selected
// metadata and rendering. It discards output; benchmark reports contain no data.
func BenchmarkCommandScale(b *testing.B) {
	for _, c := range storagetest.ScaleCases() {
		b.Run(c.Name(), func(b *testing.B) { benchmarkListCase(b, c) })
	}
}

func BenchmarkCommandLatency(b *testing.B) {
	for _, state := range []string{"cold", "warm", "changed"} {
		c := storagetest.BenchCase{Sessions: 80, Limit: 50, CacheState: state, Delay: time.Millisecond}
		b.Run(c.Name(), func(b *testing.B) { benchmarkListCase(b, c) })
	}
}

func benchmarkEnv(b *testing.B, home string, store storage.ObjectStore) Env {
	b.Helper()
	cfg := config.Config{MachineID: "bench", Storage: credentialsTestConfig(), Archive: archive.Config{SchemaVersion: 1, MachineID: "bench", Enabled: true}}
	if err := config.Save(home, cfg); err != nil {
		b.Fatal(err)
	}
	// Read-only commands receive private homes and no host environment, repo,
	// scheduler or credentials. TestMain additionally fails closed for host calls.
	return Env{Home: func() (string, error) { return home, nil }, UserHomeDir: func() (string, error) { return home, nil }, AccountHome: func() (string, error) { return home, nil }, TempDir: func() string { return home }, Now: func() time.Time { return storagetest.BenchmarkTime.Add(24 * time.Hour) }, LookupEnv: noEnv, OS: platform.Darwin, OpenStore: func(config.Config) (storage.ObjectStore, error) { return store, nil }, WorkingDir: func() (string, error) { return "", errors.New("no benchmark working directory") }, Interrupts: noInterrupts, repoKey: func(string) string { return "" }, IsTerminal: func(any) bool { return false }}
}

func benchmarkListCase(b *testing.B, c storagetest.BenchCase) {
	b.StopTimer()
	mem := storagetest.NewMemoryStore()
	storagetest.SeedArchive(b, mem, c.Sessions, 0)
	store := storagetest.NewMeasuredStore(mem, c.Delay)
	home := b.TempDir()
	env := benchmarkEnv(b, home, store)
	args := []string{"list", "--all-projects", "--json", "--limit", fmt.Sprint(c.Limit)}
	warmEnv := env
	warmEnv.OpenStore = func(config.Config) (storage.ObjectStore, error) { return mem, nil }
	b.ReportAllocs()
	for range b.N {
		if c.CacheState == "changed" {
			mem = storagetest.NewMemoryStore()
			storagetest.SeedArchive(b, mem, c.Sessions, 0)
			store.MemoryStore = mem
		}
		if err := os.RemoveAll(filepath.Join(home, "cache")); err != nil {
			b.Fatal(err)
		}
		if c.CacheState != "cold" {
			if code := Run(args, nil, io.Discard, io.Discard, warmEnv); code != 0 {
				b.Fatal("warmup failed")
			}
		}
		if c.CacheState == "changed" {
			storagetest.SeedArchive(b, mem, c.Sessions, 1)
		}
		b.StartTimer()
		code := Run(args, nil, io.Discard, io.Discard, env)
		b.StopTimer()
		if code != 0 {
			b.Fatalf("synthetic list exit %d", code)
		}
	}
	storagetest.ReportReadMetrics(b, store.Metrics())
}

// BenchmarkUsableTerminalScreen measures the first complete interactive stats
// frame. TerminalSize is requested after data loading/computation and before
// rendering. The writer marks the complete draw containing the clear-screen sequence, so a
// spinner or partial frame cannot count as usable. This is an injected 80x24
// terminal; it excludes process exec and a real terminal emulator's paint time.
func BenchmarkUsableTerminalScreen(b *testing.B) {
	b.StopTimer()
	mem := storagetest.NewMemoryStore()
	storagetest.SeedArchive(b, mem, 800, 0)
	store := storagetest.NewMeasuredStore(mem, 0)
	home := b.TempDir()
	env := benchmarkEnv(b, home, store)
	var elapsed time.Duration
	b.ReportAllocs()
	for range b.N {
		if err := os.RemoveAll(filepath.Join(home, "cache")); err != nil {
			b.Fatal(err)
		}
		in := strings.NewReader("")
		out := &benchmarkScreen{}
		env.IsTerminal = func(stream any) bool { return stream == any(in) || stream == any(out) }
		env.TerminalSize = func(io.Writer) (int, int, bool) { return 80, 24, true }
		env.openKeys = func(io.Reader) (keyTerminal, bool) { return newFakeKeys("q"), true }
		b.StartTimer()
		out.start = time.Now()
		code := Run([]string{"stats", "--prices", goldenPrices}, in, out, io.Discard, env)
		b.StopTimer()
		if code != 0 || out.first == 0 {
			b.Fatal("no usable synthetic stats frame")
		}
		elapsed += out.first
	}
	b.ReportMetric(float64(elapsed.Nanoseconds())/float64(b.N), "usable-screen-ns/op")
	storagetest.ReportReadMetrics(b, store.Metrics())
}

type benchmarkScreen struct {
	start time.Time
	first time.Duration
}

func (s *benchmarkScreen) Write(p []byte) (int, error) {
	if s.first == 0 && bytes.Contains(p, []byte(clearScreenSequence)) {
		s.first = time.Since(s.start)
	}

	return len(p), nil
}
func (*benchmarkScreen) colorTerminal() bool { return false }

// BenchmarkCommandReads pins common read-only command paths beyond list. These
// small cold cases expose request duplication without timing a real provider.
func BenchmarkCommandReads(b *testing.B) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"show-large-parent", []string{"show", "00000001000000000000000000000001"}},
		{"show-exact", []string{"show", "00000001000000000000000000000001"}},
		{"show-short-ID", []string{"show", "00000001"}},
		{"stats-narrow", []string{"stats", "--days", "1", "--prices", goldenPrices}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.StopTimer()
			mem := storagetest.NewMemoryStore()
			storagetest.SeedArchive(b, mem, 800, 0)
			if tc.name == "show-large-parent" {
				seedBenchmarkParent(b, mem, 50)
			}
			if tc.name == "stats-narrow" {
				seedBenchmarkStats(b, mem, 800)
			}
			store := storagetest.NewMeasuredStore(mem, 0)
			home := b.TempDir()
			env := benchmarkEnv(b, home, store)
			b.ReportAllocs()
			for range b.N {
				if err := os.RemoveAll(filepath.Join(home, "cache")); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				code := Run(tc.args, nil, io.Discard, io.Discard, env)
				b.StopTimer()
				if code != 0 {
					b.Fatalf("synthetic command exit %d", code)
				}
			}
			storagetest.ReportReadMetrics(b, store.Metrics())
		})
	}
}

func seedBenchmarkParent(b *testing.B, mem *storagetest.MemoryStore, children int) {
	b.Helper()
	ctx := context.Background()
	parentID := fmt.Sprintf("%08x%024x", 1, 1)
	parentKey, _ := archive.MetadataObjectKey("codex", parentID)
	data, err := mem.Get(ctx, parentKey)
	if err != nil {
		b.Fatal(err)
	}
	var parent archive.Metadata
	if err = json.Unmarshal(data, &parent); err != nil {
		b.Fatal(err)
	}
	for i := range children {
		id := fmt.Sprintf("%08x%024x", i+2, i+2)
		key, _ := archive.MetadataObjectKey("codex", id)
		data, err = mem.Get(ctx, key)
		if err != nil {
			b.Fatal(err)
		}
		var child archive.Metadata
		if err = json.Unmarshal(data, &child); err != nil {
			b.Fatal(err)
		}
		child.ParentSessionID = parentID
		child.ProjectID = parent.ProjectID
		data, err = json.Marshal(child)
		if err != nil {
			b.Fatal(err)
		}
		if err = mem.Put(ctx, key, data); err != nil {
			b.Fatal(err)
		}
		parent.LinkedSessions = append(parent.LinkedSessions, archive.LinkedSessionReference{SessionID: id, Relationship: "subagent", Status: archive.LinkedSessionPublished, ObservedAt: storagetest.BenchmarkTime})
	}
	data, err = json.Marshal(parent)
	if err != nil {
		b.Fatal(err)
	}
	if err = mem.Put(ctx, parentKey, data); err != nil {
		b.Fatal(err)
	}
}

// seedBenchmarkStats spans 90 days: a one-day query includes about 1/90 of
// sessions. Model and token fields exercise aggregation as well as selection.
func seedBenchmarkStats(b *testing.B, mem *storagetest.MemoryStore, count int) {
	b.Helper()
	ctx := context.Background()
	for i := range count {
		id := fmt.Sprintf("%08x%024x", i+1, i+1)
		key, _ := archive.MetadataObjectKey("codex", id)
		data, err := mem.Get(ctx, key)
		if err != nil {
			b.Fatal(err)
		}
		var m archive.Metadata
		if err = json.Unmarshal(data, &m); err != nil {
			b.Fatal(err)
		}
		m.CapturedAt = storagetest.BenchmarkTime.Add(24*time.Hour - time.Duration(i%90)*24*time.Hour)
		m.StartedAt = m.CapturedAt
		input, output := 1000+i, 200+i%100
		m.Counts = archive.Counts{InputTokens: &input, OutputTokens: &output}
		m.ModelTokens = []archive.ModelTokens{{Model: "gpt-5", InputTokens: &input, OutputTokens: &output}}

		data, err = json.Marshal(m)
		if err != nil {
			b.Fatal(err)
		}
		if err = mem.Put(ctx, key, data); err != nil {
			b.Fatal(err)
		}
		_, etag, err := mem.GetVersioned(ctx, key)
		if err != nil {
			b.Fatal(err)
		}
		revision, err := listingindex.NewRevision(key, data, etag)
		if err != nil {
			b.Fatal(err)
		}
		if err = listingindex.PutRevision(ctx, mem, revision); err != nil {
			b.Fatal(err)
		}
	}
}
