package discovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func metadataFixture(t *testing.T, count int) (*CodexRolloutLookup, []string, string) {
	t.Helper()
	store, cfg, at, root := fixture(t)
	ids := make([]string, count)
	for i := range count {
		ids[i] = writeRollout(t, root, cfg.Archive.Projects[0].Root, at, i+1, "sessions/nested")
		path := filepath.Join(root, "sessions/nested", "rollout-2026-10-01T12-00-00-"+ids[i]+".jsonl")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		raw = []byte(strings.ReplaceAll(strings.ReplaceAll(string(raw), `"cli_version":"test"`, `"cli_version":"0.160.0"`), `"originator":"synthetic"`, `"originator":"codex-tui"`))
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lookup.CloseReadOnly(); err != nil {
			t.Error(err)
		}
	})
	return lookup, ids, root
}

func TestMetadataInventorySharesFiveSweepsAtPhysicalScale(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 1100)
	// A second copy for every logical ID exceeds the observation cache while
	// retaining all physical candidates in the explicitly requested epoch.
	if err := os.MkdirAll(filepath.Join(root, "archived_sessions/nested"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		name := "rollout-2026-10-01T12-00-00-" + id + ".jsonl"
		raw, err := os.ReadFile(filepath.Join(root, "sessions/nested", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "archived_sessions/nested", name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := any(lookup).(agentapi.CodexRolloutSliceProvider); ok {
		t.Fatal("base lookup became full mode")
	}
	view := lookup.MetadataInventory().(*metadataInventory)
	if view.started || view.charge != 0 || view != lookup.MetadataInventory() {
		t.Fatal("request performed I/O or created second view")
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	var slice agentapi.CodexRolloutSlice
	for i, id := range ids {
		if i%256 == 0 {
			if slice != nil {
				if err := slice.Close(); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			slice, err = view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
			if err != nil {
				t.Fatal(err)
			}
			if err := slice.Valid(t.Context()); err != nil {
				t.Fatalf("scale convergence gap: %v counts=%+v elapsed=%v remaining=%v", err, view.counts, time.Since(started), lookup.remaining)
			}
		}
		set, err := slice.Thread(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if !set.Complete || len(set.Candidates) != 2 {
			t.Fatalf("incomplete physical set: %+v", set)
		}
		if err := slice.Check(t.Context(), id, set.Revision); err != nil {
			t.Fatal(err)
		}
	}
	if err := slice.Close(); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	_, peak := lookup.readBudget.Charged()
	if view.counts.sweeps != 5 || view.counts.headers != 2200 || len(view.facts) != 2200 {
		t.Fatalf("unshared epoch: %+v", view.counts)
	}
	t.Logf("physical=2200 logical=1100 sweeps=%d currentSQL=%d counts=%+v chargedPeak=%d allocBytes=%d allocations=%d heap=%d elapsed=%s remaining=%s", view.counts.sweeps, lookup.queries, view.counts, peak, after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs, after.HeapAlloc, time.Since(started), lookup.remaining)
	if err := lookup.CloseReadOnly(); err != nil {
		t.Fatal(err)
	}
	used, _ := lookup.readBudget.Charged()
	if used != 0 {
		t.Fatalf("leaked shared charge %d", used)
	}
}

func TestMetadataSliceChecksCurrentDespiteSharedFilesystemProof(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 2)
	db := hintDatabase(t, root, true)
	path := filepath.Join(root, "sessions/nested", "rollout-2026-10-01T12-00-00-"+ids[0]+".jsonl")
	addHint(t, db, ids[0], path, time.Now())
	view := lookup.MetadataInventory().(*metadataInventory)
	slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = slice.Close() }()
	set, err := slice.Thread(t.Context(), ids[0])
	if err != nil || set.Current == nil {
		t.Fatal(set, err)
	}
	if err := slice.Check(t.Context(), ids[0], set.Revision); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "sessions/nested", "rollout-2026-10-01T12-00-00-"+ids[1]+".jsonl")
	if _, err := db.ExecContext(t.Context(), "UPDATE threads SET rollout_path=? WHERE id=?", other, ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := slice.Check(t.Context(), ids[0], set.Revision); agentapi.Failure(err) != agentapi.Changed {
		t.Fatalf("stale current accepted: %v", err)
	}
	if view.counts.sweeps != 1 || lookup.queries != 6 {
		t.Fatalf("filesystem reuse lost live queries: sweeps=%d SQL=%d", view.counts.sweeps, lookup.queries)
	}
}

type metadataReadRanges struct {
	reader  io.ReaderAt
	offsets []int64
	lengths []int
}

func (r *metadataReadRanges) ReadAt(p []byte, off int64) (int, error) {
	r.offsets = append(r.offsets, off)
	r.lengths = append(r.lengths, len(p))
	return r.reader.ReadAt(p, off)
}
func TestMetadataHeaderExactRangesAndResumableMaximum(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 1)
	view := lookup.MetadataInventory().(*metadataInventory)
	path := filepath.Join(root, "sessions/nested", "rollout-2026-10-01T12-00-00-"+ids[0]+".jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line := string(raw[:strings.IndexByte(string(raw), '\n')])
	line += strings.Repeat(" ", metadataHeaderBytes-len(line)-1) + "\n"
	if err := os.WriteFile(path, []byte(line+"SENSITIVE_BODY\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := view.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	source := SourceDescriptor{Kind: archive.SourceKindFile, Root: root, Locator: path, StableKey: ids[0]}
	f, err := view.open(source)
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !view.reserve(metadataScratch) {
		t.Fatal("scratch declined")
	}
	ranges := &metadataReadRanges{reader: f}
	view.cursor = &metadataCursor{file: f, reader: ranges, source: source, info: info, line: make([]byte, 0, metadataHeaderBytes)}
	view.queue = nil
	quanta := 0
	for view.cursor != nil {
		n := quanta
		quanta++
		if quanta > 65536 {
			t.Fatal("header made no bounded progress")
		}
		before := view.counts.operations
		if err := view.step(t.Context()); err != nil {
			t.Fatal(err)
		}
		if view.counts.operations-before > 1024 {
			t.Fatal("operation quantum exceeded")
		}
		if n < 63 && view.cursor == nil {
			t.Fatal("premature cursor close")
		}
	}
	if view.cursor != nil || len(ranges.offsets) != metadataHeaderBytes || view.counts.requested != metadataHeaderBytes || view.counts.returned != metadataHeaderBytes {
		t.Fatalf("maximum header failed: %+v", view.counts)
	}
	for i, off := range ranges.offsets {
		if off != int64(i) || ranges.lengths[i] != 1 {
			t.Fatalf("body/range read at %d: offset=%d length=%d", i, off, ranges.lengths[i])
		}
	}
	t.Logf("64KiB header quanta=%d operations=%d requested=%d returned=%d", quanta, view.counts.operations, view.counts.requested, view.counts.returned)
}

func TestMetadataInventoryGapsAndFailedSliceRenewal(t *testing.T) {
	for _, mode := range []string{"malformed", "unknown", "unsafe-store", "membership", "cancelled", "root-replaced"} {
		t.Run(mode, func(t *testing.T) {
			lookup, ids, root := metadataFixture(t, 1)
			view := lookup.MetadataInventory().(*metadataInventory)
			path := filepath.Join(root, "sessions/nested", "rollout-2026-10-01T12-00-00-"+ids[0]+".jsonl")
			ctx := t.Context()
			switch mode {
			case "malformed":
				if err := os.WriteFile(path, []byte("{}\nSECRET\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unknown":
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(raw), `"source":"cli"`, `"source":"future"`)), 0600); err != nil {
					t.Fatal(err)
				}
			case "unsafe-store":
				if err := os.Symlink(t.TempDir(), filepath.Join(root, "archived_sessions")); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "membership", "root-replaced":
				first, err := view.BeginValidationSlice(ctx, agentapi.CodexValidationLimits{})
				if err != nil || first.Valid(ctx) != nil {
					t.Fatal(err, first.Valid(ctx))
				}
				_ = first.Close()
				if mode == "membership" {
					if err := os.WriteFile(filepath.Join(root, "sessions/new"), []byte("fixture"), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Rename(root, root+"-old"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(root, 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			slice, err := view.BeginValidationSlice(ctx, agentapi.CodexValidationLimits{})
			if mode == "cancelled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = slice.Close() }()
			if err := slice.Valid(ctx); err == nil {
				t.Fatal("gap marked complete")
			}
			sweeps := view.counts.sweeps
			for range 20 {
				if _, err := slice.Thread(ctx, ids[0]); err == nil {
					t.Fatal("failed slice accepted")
				}
			}
			if view.counts.sweeps != sweeps {
				t.Fatal("failed slice reswept")
			}
			if mode == "membership" {
				fresh, err := view.BeginValidationSlice(ctx, agentapi.CodexValidationLimits{})
				if err != nil {
					t.Fatal(err)
				}
				_ = fresh.Close()
				if view.counts.sweeps != sweeps+1 {
					t.Fatal("renewal did not attempt one sweep")
				}
			}
			if view.cursor != nil {
				t.Fatal("failed operation retained partial header")
			}
		})
	}
}

func TestMetadataInventoryRetainsMorePhysicalCopiesThanGraphLimit(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 1)
	name := "rollout-2026-10-01T12-00-00-" + ids[0] + ".jsonl"
	raw, err := os.ReadFile(filepath.Join(root, "sessions/nested", name))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 70 {
		dir := filepath.Join(root, "archived_sessions", fmt.Sprint(i))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	view := lookup.MetadataInventory().(*metadataInventory)
	slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = slice.Close() }()
	set, err := slice.Thread(t.Context(), ids[0])
	if err != nil || !set.Complete || len(set.Candidates) != 71 {
		t.Fatal(len(set.Candidates), set.Complete, err)
	}
	if archive.MaxHistorySpans != 64 {
		t.Fatal("graph cap silently changed")
	}
}
