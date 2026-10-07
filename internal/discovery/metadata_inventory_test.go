package discovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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
	testMetadataInventoryScale(t, false, 1100, 256)
}
func TestMetadataInventorySharesFiveSweepsWithIndexedWAL(t *testing.T) {
	testMetadataInventoryScale(t, true, 1100, 256)
}
func testMetadataInventoryScale(t *testing.T, indexed bool, logical, pairs int) {
	t.Helper()
	lookup, ids, root := metadataFixture(t, logical)
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
	if indexed {
		db := hintDatabase(t, root, true)
		for _, id := range ids {
			addHint(t, db, id, filepath.Join(root, "sessions/nested", "rollout-2026-10-01T12-00-00-"+id+".jsonl"), time.Now())
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
		if i%pairs == 0 {
			if slice != nil {
				if err := slice.Close(); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			phaseStart := time.Now()
			slice, err = view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
			t.Logf("slice=%d acquisition/validation=%s elapsed=%s fullRemaining=%s counts=%+v queries=%d", i/pairs+1, time.Since(phaseStart), time.Since(started), view.remaining, view.counts, lookup.queries)
			if err != nil {
				t.Fatal(err)
			}
			if err := slice.Valid(t.Context()); err != nil {
				t.Fatalf("scale convergence gap: %v counts=%+v elapsed=%v remaining=%v", err, view.counts, time.Since(started), lookup.remaining)
			}
		}
		set, err := slice.Thread(t.Context(), id)
		if err != nil {
			metadataScaleFailure(t, "thread", err, started, lookup, view)
		}
		if !set.Complete || len(set.Candidates) != 2 {
			t.Fatalf("incomplete physical set: %+v", set)
		}
		if err := slice.Check(t.Context(), id, set.Revision); err != nil {
			metadataScaleFailure(t, "check", err, started, lookup, view)
		}
	}
	if err := slice.Close(); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	_, peak := lookup.readBudget.Charged()
	if indexed && lookup.queries != logical*4 {
		t.Fatalf("live current SQL=%d", lookup.queries)
	}
	if view.counts.sweeps != (logical+pairs-1)/pairs || view.counts.headers != logical*2 || len(view.facts) != logical*2 {
		t.Fatalf("unshared epoch: %+v", view.counts)
	}
	t.Logf("physical=%d logical=%d sweeps=%d currentSQL=%d counts=%+v chargedPeak=%d allocBytes=%d allocations=%d heap=%d elapsed=%s ordinaryRemaining=%s", logical*2, logical, view.counts.sweeps, lookup.queries, view.counts, peak, after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs, after.HeapAlloc, time.Since(started), lookup.remaining)
	t.Logf("fullRemaining=%s retained=%d headerwork=%d", view.remaining, view.charge, view.headerBytes)
	for _, index := range lookup.indexes {
		if index.snapshot != nil {
			t.Logf("indexCopy=%+v", index.snapshot.metrics)
		}
	}
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
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	quanta := 0
	var maxStep time.Duration
	for view.cursor != nil {
		n := quanta
		quanta++
		if quanta > 65536 {
			t.Fatal("header made no bounded progress")
		}
		before := view.counts.operations
		started := time.Now()
		if err := view.step(ctx); err != nil {
			t.Fatal(err)
		}
		maxStep = max(maxStep, time.Since(started))
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
	t.Logf("64KiB header quanta=%d operations=%d requested=%d returned=%d maxStep=%s callerLiveness=30s", quanta, view.counts.operations, view.counts.requested, view.counts.returned, maxStep)
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
			if mode == "malformed" && view.counts.physical != 1 {
				t.Fatal("invalid header evaded physical acquisition count")
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

func TestMetadataFullEpochKeepsOrdinaryDeadlineIndependent(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 1)
	db := hintDatabase(t, root, true)
	path := filepath.Join(root, "sessions/nested", "rollout-2026-10-01T12-00-00-"+ids[0]+".jsonl")
	addHint(t, db, ids[0], path, time.Now())
	// Build the shared projection on the ordinary lane first, then expire that
	// lane. The full view still uses its caller context and the same projection.
	if _, err := lookup.Thread(t.Context(), ids[0]); err != nil {
		t.Fatal(err)
	}
	private := lookup.indexes[root].snapshot.path
	lookup.remaining = 0
	lookup.deadline = time.Now().Add(-time.Second)
	view := lookup.MetadataInventory().(*metadataInventory)
	slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	set, err := slice.Thread(t.Context(), ids[0])
	if err != nil || set.Current == nil {
		t.Fatal(set, err)
	}
	if lookup.indexes[root].snapshot.path != private || lookup.remaining != 0 {
		t.Fatal("full lane changed ordinary allowance or projection")
	}
	remaining := view.remaining
	_ = slice.Close()
	slice, err = view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{Duration: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_ = slice.Close()
	if view.remaining >= remaining || view.remaining > 30*time.Second {
		t.Fatal("slice renewed epoch allowance")
	}
	if _, err := lookup.Thread(t.Context(), ids[0]); agentapi.Failure(err) != agentapi.Limit {
		t.Fatal("full projection extended ordinary lane", err)
	}
}

func TestMetadataInventoryMaximumCopiesAndJointCapacity(t *testing.T) {
	store, _, _, root := fixture(t)
	id := "00000000-0000-0000-0000-000000000001"
	dir := filepath.Join(root, "sessions/nested")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"session_meta","timestamp":"2026-10-01T12:00:00Z","payload":{"id":"` + id + `","cwd":"/synthetic","source":"cli","cli_version":"0.160.0","originator":"codex-tui"}}` + "\n"
	for i := range 16384 {
		name := fmt.Sprintf("rollout-%05d-%s.jsonl", i, id)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(line+"SECRET_BODY\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lookup.CloseReadOnly() }()
	view := lookup.MetadataInventory().(*metadataInventory)
	started := time.Now()
	slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = slice.Close() }()
	if err := slice.Valid(t.Context()); err != nil {
		if !raceEnabled || agentapi.Failure(err) != agentapi.Limit || view.remaining > 0 || t.Context().Err() != nil {
			t.Fatalf("maximum fixture gap: %v physical=%d retained=%d header=%d remaining=%s", err, len(view.facts), view.charge, view.headerBytes, view.remaining)
		}
		set, refusal := slice.Thread(t.Context(), id)
		if agentapi.Failure(refusal) != agentapi.Limit || set.Complete || len(set.Candidates) != 0 || view.complete || view.cursor != nil || view.counts.physical > 16384 || view.counts.requested > int64(len(line)*view.counts.physical) {
			t.Fatal("race refusal retained partial authority", set, refusal)
		}
		t.Logf("instrumented maximum owned refusal elapsed=%s remaining=%s retained=%d headerwork=%d counts=%+v", time.Since(started), view.remaining, view.charge, view.headerBytes, view.counts)
		if err := slice.Close(); err != nil {
			t.Fatal(err)
		}
		if err := lookup.CloseReadOnly(); err != nil {
			t.Fatal(err)
		}
		if used, _ := lookup.readBudget.Charged(); used != 0 {
			t.Fatal("maximum refusal leaked shared charge", used)
		}
		return
	}
	if len(view.facts) != 16384 || view.counts.physical != 16384 || view.counts.entries != 16385 || !view.complete {
		t.Fatal("maximum physical metadata truncated")
	}
	used, peak := lookup.readBudget.Charged()
	t.Logf("maxcopies=16384 headerLen=%d retained=%d headerwork=%d charged=%d peak=%d elapsed=%s remaining=%s counts=%+v", len(line), view.charge, view.headerBytes, used, peak, time.Since(started), view.remaining, view.counts)
	// Joint capacity refusal precedes allocating a candidate result whose source
	// metadata already consumes the aggregate cap; it does not discard facts.
	if !view.reserve(metadataBytes - view.charge - view.headerBytes) {
		t.Fatal("boundary charge failed")
	}
	if _, err := slice.Thread(t.Context(), id); agentapi.Failure(err) != agentapi.Limit {
		t.Fatal("joint cap did not refuse result allocation", err)
	}
	if len(view.facts) != 16384 || view.counts.requested != int64(len(line)*16384) {
		t.Fatal("capacity refusal lost physical facts or metadata read body")
	}
	if err := slice.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lookup.CloseReadOnly(); err != nil {
		t.Fatal(err)
	}
	if used, _ := lookup.readBudget.Charged(); used != 0 {
		t.Fatal("maximum success leaked charge", used)
	}
}

func TestMetadataInventoryLateRequestDoesNotPromoteRequestedCache(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 3)
	// Request coverage after a round already started. A disposable hint map is
	// deliberately incomplete and must not supply full epoch completeness.
	lookup.coverage.Directories["midround"] = coverageDirectory{}
	lookup.coverage.request(ids[0])
	view := lookup.MetadataInventory().(*metadataInventory)
	if view.started {
		t.Fatal("late request performed inventory I/O")
	}
	slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = slice.Close() }()
	for _, id := range ids {
		set, err := slice.Thread(t.Context(), id)
		if err != nil || !set.Complete || len(set.Candidates) != 1 {
			t.Fatal(set, err)
		}
	}
	if len(view.facts) != 3 || lookup.coverage.Phase == coverageComplete {
		t.Fatal("full view used requested proof or cache")
	}
	if err := os.WriteFile(filepath.Join(root, "archived_sessions"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	fresh, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fresh.Close() }()
	if fresh.Valid(t.Context()) == nil {
		t.Fatal("missing store proof accepted replacement")
	}
}

type metadataInterruptRead struct {
	reader io.ReaderAt
	once   func()
}

func (r *metadataInterruptRead) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.reader.ReadAt(p, off)
	if r.once != nil {
		callback := r.once
		r.once = nil
		callback()
	}
	return n, err
}
func TestMetadataPartialHeaderRejectsRewriteReplacementAndCancellation(t *testing.T) {
	for _, mode := range []string{"rewrite", "replace", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			lookup, ids, root := metadataFixture(t, 1)
			view := lookup.MetadataInventory().(*metadataInventory)
			path := filepath.Join(root, "sessions/nested", "rollout-2026-10-01T12-00-00-"+ids[0]+".jsonl")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := view.start(t.Context()); err != nil {
				t.Fatal(err)
			}
			view.queue = nil
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
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reader := &metadataInterruptRead{reader: f, once: func() {
				switch mode {
				case "cancel":
					cancel()
				case "replace":
					if err := os.Rename(path, path+".old"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, raw, 0600); err != nil {
						t.Fatal(err)
					}
				case "rewrite":
					if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(raw), "codex-tui", "codex-xyz")), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}}
			view.cursor = &metadataCursor{file: f, reader: reader, source: source, info: info, line: make([]byte, 0, metadataHeaderBytes)}
			if err := view.ensure(ctx); err == nil {
				t.Fatal("interrupted header accepted")
			}
			if view.cursor != nil || view.complete || len(view.facts) != 0 {
				t.Fatal("partial authority retained")
			}
			newline := strings.IndexByte(string(raw), '\n')
			if view.counts.requested > int64(newline+1) {
				t.Fatal("metadata read requested body")
			}
		})
	}
}
func TestMetadataApprovedAliasAndComponentRetargetRefuse(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 1)
	alias := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	lookup.homes = []string{alias}
	view := lookup.MetadataInventory().(*metadataInventory)
	if err := view.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), alias); err != nil {
		t.Fatal(err)
	}
	if err := view.ensure(t.Context()); err == nil {
		t.Fatal("approved spelling retarget accepted")
	}
	other, otherIDs, otherRoot := metadataFixture(t, 1)
	otherView := other.MetadataInventory().(*metadataInventory)
	if err := otherView.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(otherRoot, "sessions/nested")
	if err := os.Rename(nested, nested+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(nested+".old", nested); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(nested, "rollout-2026-10-01T12-00-00-"+otherIDs[0]+".jsonl")
	source := SourceDescriptor{Kind: archive.SourceKindFile, Root: otherRoot, Locator: path, StableKey: ids[0]}
	if file, err := otherView.open(source); err == nil {
		_ = file.Close()
		t.Fatal("raced intermediate symlink accepted")
	}
}
func TestMetadataConflictingBodiesRemainPhysicalFacts(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 1)
	name := "rollout-2026-10-01T12-00-00-" + ids[0] + ".jsonl"
	raw, err := os.ReadFile(filepath.Join(root, "sessions/nested", name))
	if err != nil {
		t.Fatal(err)
	}
	header := raw[:strings.IndexByte(string(raw), '\n')+1]
	dir := filepath.Join(root, "archived_sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), append(append([]byte{}, header...), []byte("DIFFERENT_BODY\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	view := lookup.MetadataInventory().(*metadataInventory)
	slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = slice.Close() }()
	set, err := slice.Thread(t.Context(), ids[0])
	if err != nil || !set.Complete || len(set.Candidates) != 2 {
		t.Fatal(set, err)
	}
	if view.counts.returned != 2*int64(len(header)) {
		t.Fatal("body read or copy discarded")
	}
}
func TestMetadataFullProjectionDoesNotExtendOrdinaryLane(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 1)
	db := hintDatabase(t, root, true)
	path := filepath.Join(root, "sessions/nested", "rollout-2026-10-01T12-00-00-"+ids[0]+".jsonl")
	addHint(t, db, ids[0], path, time.Now())
	view := lookup.MetadataInventory().(*metadataInventory)
	slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = slice.Thread(t.Context(), ids[0])
	if err != nil {
		t.Fatal(err)
	}
	_ = slice.Close()
	private := lookup.indexes[root].snapshot.path
	if lookup.remaining != Budget {
		t.Fatal("full work consumed ordinary allowance")
	}
	if _, err := lookup.Thread(t.Context(), ids[0]); err != nil {
		t.Fatal(err)
	}
	if lookup.indexes[root].snapshot.path != private || lookup.remaining >= Budget {
		t.Fatal("ordinary lane did not reuse projection within own allowance")
	}
	lookup.remaining = 0
	if _, err := lookup.Thread(t.Context(), ids[0]); agentapi.Failure(err) != agentapi.Limit {
		t.Fatal("full view extended ordinary time", err)
	}
}

func TestMetadataStableFactsConflictAndMultipleHomes(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(strconv.FormatBool(conflict), func(t *testing.T) {
			lookup, ids, root := metadataFixture(t, 1)
			other := t.TempDir()
			name := "rollout-2026-10-01T12-00-00-" + ids[0] + ".jsonl"
			raw, err := os.ReadFile(filepath.Join(root, "sessions/nested", name))
			if err != nil {
				t.Fatal(err)
			}
			if conflict {
				raw = []byte(strings.ReplaceAll(string(raw), `"originator":"codex-tui"`, `"originator":"codex_cli_rs"`))
			}
			if err := os.MkdirAll(filepath.Join(other, "sessions"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(other, "sessions", name), raw, 0600); err != nil {
				t.Fatal(err)
			}
			lookup.homes = append(lookup.homes, other)
			view := lookup.MetadataInventory().(*metadataInventory)
			slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = slice.Close() }()
			set, err := slice.Thread(t.Context(), ids[0])
			if conflict {
				if err == nil || set.Complete {
					t.Fatal("conflicting stable metadata accepted", set, err)
				}
			} else if err != nil || !set.Complete || len(set.Candidates) != 2 {
				t.Fatal(set, err)
			}
		})
	}
}

func TestMetadataPhysicalCeilingRefusesBeforeExtraOpen(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 1)
	view := lookup.MetadataInventory().(*metadataInventory)
	if err := view.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	view.counts.physical = 16384
	view.batch = []SourceEntry{{Source: SourceDescriptor{Kind: archive.SourceKindFile, Root: root, Locator: filepath.Join(root, "sessions/nested", "rollout-2026-10-01T12-00-00-"+ids[0]+".jsonl")}}}
	before := view.counts.fileOpens
	if err := view.step(t.Context()); agentapi.Failure(err) != agentapi.Limit {
		t.Fatal(err)
	}
	if view.counts.fileOpens != before || view.cursor != nil || view.counts.physical != 16384 {
		t.Fatal("extra physical file opened beyond cap")
	}
}

func TestMetadataInventorySmallerIndexedScale(t *testing.T) {
	testMetadataInventoryScale(t, true, 64, 16)
}
func metadataScaleFailure(t *testing.T, phase string, err error, started time.Time, lookup *CodexRolloutLookup, view *metadataInventory) {
	t.Helper()
	var sourceErr *agentapi.SourceError
	var cause error
	if errors.As(err, &sourceErr) {
		cause = sourceErr.Err
	}
	for _, index := range lookup.indexes {
		if index.snapshot != nil {
			t.Logf("failureIndexCopy=%+v", index.snapshot.metrics)
		}
	}
	used, peak := lookup.readBudget.Charged()
	t.Fatalf("phase=%s failure=%v kind=%s cause=%v elapsed=%s fullRemaining=%s counts=%+v SQL=%d retained=%d headerwork=%d charged=%d peak=%d", phase, err, agentapi.Failure(err), cause, time.Since(started), view.remaining, view.counts, lookup.queries, view.charge, view.headerBytes, used, peak)
}

func TestMetadataOwnedDeadlineRefusesAndPreservesCaller(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 1)
	db := hintDatabase(t, root, true)
	addHint(t, db, ids[0], filepath.Join(root, "sessions/nested", "rollout-2026-10-01T12-00-00-"+ids[0]+".jsonl"), time.Now())
	view := lookup.MetadataInventory().(*metadataInventory)
	slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	view.remaining = time.Nanosecond
	set, err := slice.Thread(t.Context(), ids[0])
	if agentapi.Failure(err) != agentapi.Limit || set.Complete {
		t.Fatal("expired work did not report typed limit", set, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := slice.Thread(ctx, ids[0]); !errors.Is(err, context.Canceled) {
		t.Fatal("caller cancellation lost", err)
	}
	ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := slice.Thread(ctx, ids[0]); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("caller deadline lost", err)
	}
	var failure *agentapi.SourceError
	var cause error
	if errors.As(err, &failure) {
		cause = failure.Err
	}
	t.Logf("owned expiration kind=%s cause=%v remaining=%s queries=%d complete=%v", agentapi.Failure(err), cause, view.remaining, lookup.queries, set.Complete)
	if err := slice.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lookup.CloseReadOnly(); err != nil {
		t.Fatal(err)
	}
	if used, _ := lookup.readBudget.Charged(); used != 0 {
		t.Fatal("expired work leaked charge", used)
	}
}

func TestMetadataAliasedHomesSharePhysicalFactsButFenceEverySpelling(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 1)
	alias := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	lookup.homes = []string{root, alias}
	view := lookup.MetadataInventory().(*metadataInventory)
	slice, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	set, err := slice.Thread(t.Context(), ids[0])
	if err != nil || !set.Complete || len(set.Candidates) != 1 || view.counts.physical != 1 || view.counts.headers != 1 {
		t.Fatal("alias duplicated physical authority", set, err, view.counts)
	}
	if err := slice.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), alias); err != nil {
		t.Fatal(err)
	}
	slice, err = view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if err := slice.Valid(t.Context()); agentapi.Failure(err) != agentapi.Changed {
		t.Fatal("secondary approved alias fence lost", err)
	}
	if err := slice.Close(); err != nil {
		t.Fatal(err)
	}
}
