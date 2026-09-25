package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/state/statetest"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func readPublishedStateFile(t *testing.T, store *state.Store, id string) publishedFile {
	t.Helper()
	raw, err := os.ReadFile(publishedPath(store, id))
	if err != nil {
		t.Fatal(err)
	}
	var file publishedFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	return file
}

func writePublishedStateFile(t *testing.T, store *state.Store, id string, file publishedFile) {
	t.Helper()
	raw, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publishedPath(store, id), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func runAt(t *testing.T, store *state.Store, remote storage.ObjectStore, at time.Time) Result {
	t.Helper()
	result, err := Run(context.Background(), store, remote, Options{MachineID: "m", Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func mtime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

// Claude Code deletes transcripts after cleanupPeriodDays while this archive
// keeps sessions for 90 days, so a published session losing its transcript is
// the steady state. It must be a recorded gap, written once, that keeps the
// published snapshot and clears on its own if the file ever comes back.
func TestMissingTranscriptBlocksOnceKeepsSnapshotAndRecovers(t *testing.T) {
	dir := t.TempDir()
	local := newTestStore(t)
	remote := storage.NewMemoryStore()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	reg := registration(t, path)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	runAt(t, local, remote, t0)
	published, publishedAt, _, err := local.LoadLastPublished(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	metadataBefore := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// A stop hook fired just before the application cleaned up.
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	result := runAt(t, local, remote, t0.Add(time.Hour))
	if len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("a missing transcript must be a gap, not an error: %#v", result)
	}
	if reason, blocked, err := local.LoadBlocked(reg.ArchiveSessionID); err != nil || !blocked || reason != state.BlockedReasonTranscriptMissing {
		t.Fatalf("reason=%q blocked=%t err=%v", reason, blocked, err)
	}
	if requests, _ := local.LoadRequests(); len(requests) != 0 {
		t.Fatalf("the request was left pending forever: %#v", requests)
	}
	status, err := local.LoadStatus()
	if err != nil || status.LastError != "" || status.PendingCount != 0 {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	kept, keptAt, found, err := local.LoadLastPublished(reg.ArchiveSessionID)
	if err != nil || !found || !keptAt.Equal(publishedAt) {
		t.Fatalf("the last published snapshot was not kept: found=%t at=%s err=%v", found, keptAt, err)
	}
	if same, _ := bundleEvidenceEqual(kept, published); !same {
		t.Fatal("the last published snapshot changed")
	}

	// Recorded once: later passes neither error nor rewrite the cache.
	cachePath := publishedPath(local, reg.ArchiveSessionID)
	before := mtime(t, cachePath)
	for pass := 2; pass <= 3; pass++ {
		if result := runAt(t, local, remote, t0.Add(time.Duration(pass)*time.Hour)); len(result.Errors) != 0 {
			t.Fatalf("pass %d: %#v", pass, result)
		}
	}
	if !mtime(t, cachePath).Equal(before) {
		t.Fatal("the gap was rewritten on every pass")
	}

	// The same file comes back: the gap is over even though nothing changed,
	// and nothing is republished.
	writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	result = runAt(t, local, remote, t0.Add(4*time.Hour))
	if len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("result=%#v", result)
	}
	if _, blocked, _ := local.LoadBlocked(reg.ArchiveSessionID); blocked {
		t.Fatal("the gap outlived the missing file")
	}
	if _, _, status, _, _ := local.LoadPublished(reg.ArchiveSessionID); status != state.CacheStatusPublished {
		t.Fatalf("status after recovery = %q, want the status the block replaced", status)
	}
	if after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID); !after.CapturedAt.Equal(metadataBefore.CapturedAt) {
		t.Fatal("recovery republished an unchanged session")
	}

	// New activity after recovery publishes as usual.
	writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"m2","payload":{"type":"message","role":"user","content":"more"}}`)
	if result := runAt(t, local, remote, t0.Add(5*time.Hour)); len(result.Published) != 1 {
		t.Fatalf("new evidence after recovery did not publish: %#v", result)
	}
}

// The transcript is missing before anything was ever captured, then appears.
func TestMissingTranscriptBeforeFirstCaptureRecoversWhenFileAppears(t *testing.T) {
	dir := t.TempDir()
	local := newTestStore(t)
	remote := storage.NewMemoryStore()
	path := filepath.Join(dir, "later.jsonl")
	reg := registration(t, path)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if result := runAt(t, local, remote, t0); len(result.Errors) != 0 {
		t.Fatalf("result=%#v", result)
	}
	if reason, blocked, _ := local.LoadBlocked(reg.ArchiveSessionID); !blocked || reason != state.BlockedReasonTranscriptMissing {
		t.Fatalf("reason=%q blocked=%t", reason, blocked)
	}
	writeTranscript(t, dir, "later.jsonl", codexTranscript)
	if result := runAt(t, local, remote, t0.Add(time.Hour)); len(result.Published) != 1 {
		t.Fatalf("the transcript appeared but was not captured: %#v", result)
	}
	if _, blocked, _ := local.LoadBlocked(reg.ArchiveSessionID); blocked {
		t.Fatal("still blocked after capture")
	}
}

// While a session sits in its normal published state, the published cache
// holds its bundle once. A newer candidate that differs gets its own copy, and
// the actually-published baseline survives it intact.
func TestPublishedCacheStoresTheBundleOnce(t *testing.T) {
	dir := t.TempDir()
	local := newTestStore(t)
	remote := storage.NewMemoryStore()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	reg := registration(t, path)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	runAt(t, local, remote, t0)

	file := readPublishedStateFile(t, local, reg.ArchiveSessionID)
	if file.LastPublished == nil || !file.LastPublished.SameAsBundle || len(file.LastPublished.Bundle.NativeRecords) != 0 {
		t.Fatalf("the published bundle is stored twice: %#v", file.LastPublished)
	}
	published, _, found, err := local.LoadLastPublished(reg.ArchiveSessionID)
	if err != nil || !found || len(published.NativeRecords) == 0 {
		t.Fatalf("the shared snapshot does not resolve: found=%t err=%v", found, err)
	}

	// A new candidate inside the upload interval is rate limited: the cache
	// now holds the candidate, and the baseline must be its own copy.
	writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"m2","payload":{"type":"message","role":"user","content":"more"}}`)
	if result := runAt(t, local, remote, t0.Add(time.Minute)); len(result.Published) != 0 {
		t.Fatalf("expected a rate-limited candidate: %#v", result)
	}
	baseline, _, found, err := local.LoadLastPublished(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(err)
	}
	if same, _ := bundleEvidenceEqual(baseline, published); !same {
		t.Fatal("the published baseline was replaced by an unpublished candidate")
	}
	candidate, _, status, _, _ := local.LoadPublished(reg.ArchiveSessionID)
	if status != state.CacheStatusRateLimited || len(candidate.NativeRecords) != len(published.NativeRecords)+1 {
		t.Fatalf("status=%q records=%d", status, len(candidate.NativeRecords))
	}
}

// State written before the shared-copy marker existed carries a full second
// copy and must keep loading.
func TestPublishedCacheReadsTheOlderTwoCopyShape(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t, "/unused")
	filtered, err := archive.CodexAdapter{}.FilterJSONL(strings.NewReader(codexTranscript + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	bundle, err := archive.NewSourceBundle(reg, archive.CodexAdapter{}, filtered, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacy := publishedFile{Bundle: bundle, PublishedAt: at, Status: state.CacheStatusPublished, LastPublished: &publishedFileSnapshot{Bundle: bundle, PublishedAt: at}}
	writePublishedStateFile(t, local, reg.ArchiveSessionID, legacy)
	got, gotAt, found, err := local.LoadLastPublished(reg.ArchiveSessionID)
	if err != nil || !found || !gotAt.Equal(at) {
		t.Fatalf("found=%t at=%s err=%v", found, gotAt, err)
	}
	if same, _ := bundleEvidenceEqual(got, bundle); !same {
		t.Fatal("legacy snapshot did not load")
	}
}

// settledSession publishes one session and returns its registration and the
// transcript's path. The pass after a publish leaves a scan signature.
func settledSession(t *testing.T, local *state.Store, content string) archive.SessionRegistration {
	t.Helper()
	path := writeTranscript(t, t.TempDir(), "codex.jsonl", content)
	reg := registration(t, path)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	runAt(t, local, storage.NewMemoryStore(), time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC))
	return reg
}

func TestUnchangedCheckSaysYesOnlyWhenNothingIsOwed(t *testing.T) {
	opts := Options{MachineID: "m"}
	check := func(t *testing.T, local *state.Store, reg archive.SessionRegistration, o Options) bool {
		t.Helper()
		unchanged, err := unchangedSinceLastScan(context.Background(), local, reg, o)
		if err != nil {
			t.Fatal(err)
		}
		return unchanged
	}

	t.Run("settled and untouched", func(t *testing.T) {
		local := newTestStore(t)
		reg := settledSession(t, local, codexTranscript)
		if !check(t, local, reg, opts) {
			t.Fatal("a settled, untouched transcript was not recognized")
		}
	})
	t.Run("same size, mtime moved by one nanosecond", func(t *testing.T) {
		local := newTestStore(t)
		reg := settledSession(t, local, codexTranscript)
		modified := mtime(t, reg.TranscriptPath).Add(time.Nanosecond)
		if err := os.Chtimes(reg.TranscriptPath, modified, modified); err != nil {
			t.Fatal(err)
		}
		if mtime(t, reg.TranscriptPath).UnixNano() == modified.Add(-time.Nanosecond).UnixNano() {
			t.Skip("this filesystem does not store nanosecond modification times")
		}
		if check(t, local, reg, opts) {
			t.Fatal("a same-second rewrite was skipped")
		}
	})
	t.Run("grew", func(t *testing.T) {
		local := newTestStore(t)
		reg := settledSession(t, local, codexTranscript)
		f, err := os.OpenFile(reg.TranscriptPath, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString("\n")
		_ = f.Close()
		if check(t, local, reg, opts) {
			t.Fatal("a grown transcript was skipped")
		}
	})
	t.Run("parser upgrade", func(t *testing.T) {
		local := newTestStore(t)
		reg := settledSession(t, local, codexTranscript)
		if check(t, local, reg, Options{MachineID: "m", ParserVersion: "next"}) {
			t.Fatal("a parser upgrade skipped re-derivation")
		}
	})
	t.Run("pending publication", func(t *testing.T) {
		local := newTestStore(t)
		reg := settledSession(t, local, codexTranscript)
		if err := local.SavePending(reg.ArchiveSessionID, state.PendingPublication{SourceKey: "k", MetadataKey: "m", SourceSHA256: "s", SourceBytes: []byte{1}, MetadataBytes: []byte{1}}); err != nil {
			t.Fatal(err)
		}
		if check(t, local, reg, opts) {
			t.Fatal("a pending publication was skipped")
		}
	})
	t.Run("interrupted scan", func(t *testing.T) {
		local := newTestStore(t)
		reg := settledSession(t, local, codexTranscript)
		if err := local.SetScanPending(reg.ArchiveSessionID, true); err != nil {
			t.Fatal(err)
		}
		if check(t, local, reg, opts) {
			t.Fatal("an interrupted scan was skipped")
		}
	})
	t.Run("blocked", func(t *testing.T) {
		local := newTestStore(t)
		reg := settledSession(t, local, codexTranscript)
		if _, err := blockSession(local, reg.ArchiveSessionID, state.Request{}, state.BlockedReasonTranscriptRewritten, nil); err != nil {
			t.Fatal(err)
		}
		if check(t, local, reg, opts) {
			t.Fatal("a recorded gap was skipped")
		}
	})
	t.Run("subagent owing its parent a link", func(t *testing.T) {
		local := newTestStore(t)
		reg := settledSession(t, local, codexTranscript)
		reg.ParentSessionID = "parent"
		if check(t, local, reg, opts) {
			t.Fatal("a subagent that still owes its parent a link was skipped")
		}
	})
	t.Run("subagent its parent links", func(t *testing.T) {
		local := newTestStore(t)
		reg := settledSession(t, local, codexTranscript)
		reg.ParentSessionID = "parent"
		parent := archive.SourceBundle{
			ArchiveSessionID: "parent", Capture: archive.SourceCapture{Harness: archive.Harness{Name: "codex"}},
			LinkedSessions: []archive.LinkedSessionReference{{SessionID: reg.ArchiveSessionID, Status: archive.LinkedSessionPublished}},
		}
		if err := statetest.SavePublished(local, "parent", parent, time.Time{}, state.CacheStatusPublished); err != nil {
			t.Fatal(err)
		}
		if !check(t, local, reg, opts) {
			t.Fatal("a settled subagent its parent already links was read again")
		}
	})
	t.Run("cursor text is never trusted to a stat", func(t *testing.T) {
		local := newTestStore(t)
		reg := settledSession(t, local, codexTranscript)
		signature, _, _ := local.LoadScanSignature(reg.ArchiveSessionID)
		signature.SourceFormat = cursorTextSourceFormat
		if err := local.SaveScanSignature(reg.ArchiveSessionID, signature); err != nil {
			t.Fatal(err)
		}
		if check(t, local, reg, opts) {
			t.Fatal("a Cursor text transcript was skipped on a stat alone")
		}
	})
	// The documented residual risk, pinned so it cannot silently widen: an
	// in-place rewrite to the same byte length that also restores the exact
	// nanosecond mtime is indistinguishable by stat, and is skipped.
	t.Run("residual risk: same size and same nanosecond", func(t *testing.T) {
		local := newTestStore(t)
		reg := settledSession(t, local, codexTranscript)
		original := mtime(t, reg.TranscriptPath)
		rewritten := strings.Replace(codexTranscript, "visible", "VISIBLE", 1)
		if err := os.WriteFile(reg.TranscriptPath, []byte(rewritten), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(reg.TranscriptPath, original, original); err != nil {
			t.Fatal(err)
		}
		if !check(t, local, reg, opts) {
			t.Fatal("the residual-risk case changed; update the ledger's description of it")
		}
	})
}

// A same-length in-place rewrite that moves the mtime by any amount is read
// and recorded as the gap it is.
func TestSameLengthRewriteIsDetected(t *testing.T) {
	local := newTestStore(t)
	remote := storage.NewMemoryStore()
	reg := settledSession(t, local, codexTranscript)
	original := mtime(t, reg.TranscriptPath)
	rewritten := strings.Replace(codexTranscript, "visible", "VISIBLE", 1)
	if err := os.WriteFile(reg.TranscriptPath, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	moved := original.Add(time.Nanosecond)
	if err := os.Chtimes(reg.TranscriptPath, moved, moved); err != nil {
		t.Fatal(err)
	}
	if mtime(t, reg.TranscriptPath).Equal(original) {
		t.Skip("this filesystem does not store nanosecond modification times")
	}
	result := runAt(t, local, remote, time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC))
	if len(result.Errors) != 0 {
		t.Fatalf("result=%#v", result)
	}
	if reason, blocked, _ := local.LoadBlocked(reg.ArchiveSessionID); !blocked || reason != state.BlockedReasonTranscriptRewritten {
		t.Fatalf("an in-place rewrite was not detected: reason=%q blocked=%t", reason, blocked)
	}
}

// fileState is everything a write could change about one path.
type fileState struct {
	size  int64
	mtime int64
}

// snapshotLocalState records every file and directory under home, except the
// pass-level status file (one write per pass, not per session) and home
// itself, whose mtime that write moves. Directory mtimes are included so a
// file created and removed within the pass (the scan journal) is still seen.
func snapshotLocalState(t *testing.T, home string) map[string]fileState {
	t.Helper()
	out := map[string]fileState{}
	err := filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == home || filepath.Base(path) == "status.json" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		out[path] = fileState{size: info.Size(), mtime: info.ModTime().UnixNano()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// largeCodexTranscript builds a synthetic transcript of about size bytes.
func largeCodexTranscript(size int) string {
	var b strings.Builder
	b.WriteString(`{"type":"turn_context","model":"gpt-test"}` + "\n")
	filler := strings.Repeat("synthetic filler text ", 40)
	for i := 0; b.Len() < size; i++ {
		fmt.Fprintf(&b, `{"type":"response_item","id":"m%d","payload":{"type":"message","role":"assistant","content":%q}}`+"\n", i, filler)
	}
	return b.String()
}

// The plan's target: 300 unchanged sessions of 400 KB each in well under one
// second per pass, with no per-session journal, cache, or signature writes
// and no reads of the published cache at all.
//
// The invariants (nothing read, nothing written) are checked on every run,
// on 20 sessions of 64 KB. The documented number needs the full 300 sessions
// (setup alone publishes 120 MB, most of the package's test time) and no race
// detector, so it runs only with AGENT_ARCHIVE_PERF=1 in a plain build: CI
// has a step for exactly that, since its main run uses -race.
func TestUnchangedSessionsCostNoWritesAndStayFast(t *testing.T) {
	sessions, size, timed := 20, 64*1024, false
	if os.Getenv(perfEnv) != "" {
		if raceEnabled {
			t.Skip("the wall-clock target is for a plain build, not the race detector")
		}
		sessions, size, timed = 300, 400*1024, true
	}
	home := t.TempDir()
	local, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	remote := storage.NewMemoryStore()
	dir := t.TempDir()
	content := largeCodexTranscript(size)
	for i := range sessions {
		reg := registration(t, writeTranscript(t, dir, fmt.Sprintf("s%03d.jsonl", i), content))
		reg.ArchiveSessionID = fmt.Sprintf("session-%03d", i)
		reg.NativeSessionID = fmt.Sprintf("native-%03d", i)
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if result := runAt(t, local, remote, t0); len(result.Published) != sessions || len(result.Errors) != 0 {
		t.Fatalf("setup pass: published=%d errors=%v", len(result.Published), result.Errors)
	}

	// Prove the unchanged pass never reads the published cache: make every
	// cache file unreadable. Any attempt to open one fails the session.
	cacheFiles, _ := filepath.Glob(filepath.Join(home, "published", "*.json"))
	if len(cacheFiles) != sessions {
		t.Fatalf("cache files=%d", len(cacheFiles))
	}
	unreadable := os.Geteuid() != 0
	if unreadable {
		for _, path := range cacheFiles {
			if err := os.Chmod(path, 0o000); err != nil {
				t.Fatal(err)
			}
		}
	}
	before := snapshotLocalState(t, home)
	start := time.Now()
	result := runAt(t, local, remote, t0.Add(time.Hour))
	elapsed := time.Since(start)
	if unreadable {
		for _, path := range cacheFiles {
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if result.Scanned != sessions || len(result.Skipped) != sessions || len(result.Published) != 0 || len(result.Errors) != 0 {
		t.Fatalf("unchanged pass: scanned=%d skipped=%d published=%d errors=%v", result.Scanned, len(result.Skipped), len(result.Published), result.Errors)
	}
	after := snapshotLocalState(t, home)
	for path, state := range after {
		if before[path] != state {
			t.Errorf("unchanged pass wrote %s", path)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			t.Errorf("unchanged pass removed %s", path)
		}
	}

	// For comparison, the same pass without the short-circuit: remove the
	// signatures of up to 20 sessions so they are read, filtered, compared,
	// and journaled. (Twenty prove the detector; all 300 would add a minute.)
	signatures, _ := filepath.Glob(filepath.Join(home, "scan-signatures", "*.json"))
	signatures = signatures[:min(len(signatures), 20)]
	for _, path := range signatures {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	beforeFull := snapshotLocalState(t, home)
	start = time.Now()
	runAt(t, local, remote, t0.Add(2*time.Hour))
	fullScan := time.Since(start)
	// The detector itself must be able to see a full pass's writes (journal
	// entries created and removed, signatures rewritten); otherwise the
	// no-writes assertion above would prove nothing.
	changed := 0
	for path, state := range snapshotLocalState(t, home) {
		if beforeFull[path] != state {
			changed++
		}
	}
	if changed < len(signatures) {
		t.Fatalf("the snapshot saw only %d changed paths after a full pass over %d sessions", changed, len(signatures))
	}

	t.Logf("%d unchanged sessions of %d KB: short-circuit pass %s (%.2f ms/session); full re-read of %d %s", sessions, size/1024, elapsed, float64(elapsed.Microseconds())/1000/float64(sessions), len(signatures), fullScan)
	if timed && elapsed >= time.Second {
		t.Fatalf("an unchanged pass took %s, want well under one second", elapsed)
	}
}

// perfEnv, set to anything, runs the performance tests at their documented
// size with their wall-clock assertions (see
// TestUnchangedSessionsCostNoWritesAndStayFast).
const perfEnv = "AGENT_ARCHIVE_PERF"

// The on-disk published cache after one publication, for the ledger: the
// shared-copy marker roughly halves it.
func TestPublishedCacheSizeIsAboutOneBundle(t *testing.T) {
	local := newTestStore(t)
	reg := settledSession(t, local, largeCodexTranscript(64*1024))
	raw, err := os.ReadFile(publishedPath(local, reg.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	bundle, _, _, _, err := local.LoadPublished(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > len(encoded)*3/2 {
		t.Fatalf("published cache is %d bytes for a %d-byte bundle; the bundle is stored more than once", len(raw), len(encoded))
	}
	if !bytes.Contains(raw, []byte(`"same_as_bundle": true`)) {
		t.Fatal("the shared-copy marker is missing")
	}
}

func evidenceKinds(evidence []archive.SupplementalEvidence) []string {
	var kinds []string
	for _, item := range evidence {
		kinds = append(kinds, string(item.Kind))
	}
	return kinds
}

// A stop hook's final response can land moments before the application
// deletes the transcript. Blocking acknowledges that request, so without care
// the response is dropped on the floor. Instead the block holds it, and it
// publishes when the file returns, even when the file returns byte-identical.
func TestHookEvidenceHeldWhileTranscriptMissingPublishesOnReturn(t *testing.T) {
	dir := t.TempDir()
	local := newTestStore(t)
	remote := storage.NewMemoryStore()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	reg := registration(t, path)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	runAt(t, local, remote, t0)
	metadataBefore := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	final := archive.SupplementalEvidence{Kind: archive.EvidenceKindFinalResponse, ObservedAt: t0.Add(time.Hour), Provenance: "hook", Payload: map[string]any{"turn_id": "last"}}
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", t0.Add(time.Hour), final); err != nil {
		t.Fatal(err)
	}
	if result := runAt(t, local, remote, t0.Add(time.Hour)); len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("result=%#v", result)
	}
	if requests, _ := local.LoadRequests(); len(requests) != 0 {
		t.Fatalf("the request was left pending: %#v", requests)
	}
	if file := readPublishedStateFile(t, local, reg.ArchiveSessionID); len(file.DeferredHookEvidence) != 1 {
		t.Fatalf("the acknowledged evidence was dropped: %#v", file.DeferredHookEvidence)
	}

	// Still missing: a later request accumulates, and passes with nothing new
	// neither error nor grow the held evidence.
	feedback := archive.SupplementalEvidence{Kind: archive.EvidenceKindExplicitFeedback, ObservedAt: t0.Add(2 * time.Hour), Provenance: "user:agent-archive-feedback-file", Payload: map[string]any{"event_id": "fb-1", "text": "good", "source": "user"}}
	if err := local.SaveRequest(reg.ArchiveSessionID, "explicit_feedback", t0.Add(2*time.Hour), feedback); err != nil {
		t.Fatal(err)
	}
	for pass := 2; pass <= 3; pass++ {
		if result := runAt(t, local, remote, t0.Add(time.Duration(pass)*time.Hour)); len(result.Errors) != 0 || len(result.Published) != 0 {
			t.Fatalf("pass %d: %#v", pass, result)
		}
	}
	if file := readPublishedStateFile(t, local, reg.ArchiveSessionID); len(file.DeferredHookEvidence) != 2 {
		t.Fatalf("held evidence = %v, want the final response and the feedback", evidenceKinds(file.DeferredHookEvidence))
	}

	// The identical file returns: the held evidence is what gets published.
	writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	result := runAt(t, local, remote, t0.Add(4*time.Hour))
	if len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("the held evidence did not publish with the recovery: %#v", result)
	}
	if requests, _ := local.LoadRequests(); len(requests) != 0 {
		t.Fatalf("the replayed request was not acknowledged: %#v", requests)
	}
	published, _, found, err := local.LoadLastPublished(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatalf("found=%t err=%v", found, err)
	}
	kinds := strings.Join(evidenceKinds(published.SupplementalEvidence), ",")
	if !strings.Contains(kinds, string(archive.EvidenceKindFinalResponse)) || !strings.Contains(kinds, string(archive.EvidenceKindExplicitFeedback)) {
		t.Fatalf("published evidence = %s", kinds)
	}
	file := readPublishedStateFile(t, local, reg.ArchiveSessionID)
	if file.Status != state.CacheStatusPublished || len(file.DeferredHookEvidence) != 0 {
		t.Fatalf("status=%q held=%d after recovery", file.Status, len(file.DeferredHookEvidence))
	}
	if after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID); !after.CapturedAt.After(metadataBefore.CapturedAt) {
		t.Fatal("new evidence did not move the capture time")
	}

	// Nothing is owed any more: the next pass is a stat skip.
	if result := runAt(t, local, remote, t0.Add(5*time.Hour)); len(result.Published) != 0 || len(result.Skipped) != 1 {
		t.Fatalf("result=%#v", result)
	}
}

// The same, before anything was ever captured: the block has no previous
// status to restore, so the first real candidate replaces it, and the held
// evidence must still ride along.
func TestHookEvidenceHeldBeforeFirstCapturePublishesWhenFileAppears(t *testing.T) {
	dir := t.TempDir()
	local := newTestStore(t)
	remote := storage.NewMemoryStore()
	reg := registration(t, filepath.Join(dir, "codex.jsonl"))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	final := archive.SupplementalEvidence{Kind: archive.EvidenceKindFinalResponse, ObservedAt: t0, Provenance: "hook", Payload: map[string]any{"turn_id": "only"}}
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", t0, final); err != nil {
		t.Fatal(err)
	}
	if result := runAt(t, local, remote, t0); len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("result=%#v", result)
	}
	if file := readPublishedStateFile(t, local, reg.ArchiveSessionID); file.Status != state.CacheStatusBlocked || len(file.DeferredHookEvidence) != 1 {
		t.Fatalf("status=%q held=%d", file.Status, len(file.DeferredHookEvidence))
	}
	writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	if result := runAt(t, local, remote, t0.Add(time.Hour)); len(result.Published) != 1 || len(result.Errors) != 0 {
		t.Fatalf("result=%#v", result)
	}
	published, _, _, _ := local.LoadLastPublished(reg.ArchiveSessionID)
	if kinds := evidenceKinds(published.SupplementalEvidence); len(kinds) != 1 || kinds[0] != string(archive.EvidenceKindFinalResponse) {
		t.Fatalf("published evidence = %v", kinds)
	}
	if requests, _ := local.LoadRequests(); len(requests) != 0 {
		t.Fatalf("requests left pending: %#v", requests)
	}
	if file := readPublishedStateFile(t, local, reg.ArchiveSessionID); file.Status != state.CacheStatusPublished || len(file.DeferredHookEvidence) != 0 {
		t.Fatalf("status=%q held=%d", file.Status, len(file.DeferredHookEvidence))
	}
}
