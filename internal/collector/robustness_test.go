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
	"github.com/wangjohn/agent-archive/internal/storage"
)

func readPublishedStateFile(t *testing.T, store *LocalStore, id string) publishedState {
	t.Helper()
	raw, err := os.ReadFile(store.publishedPath(id))
	if err != nil {
		t.Fatal(err)
	}
	var state publishedState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func writePublishedStateFile(t *testing.T, store *LocalStore, id string, state publishedState) {
	t.Helper()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.publishedPath(id), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func runAt(t *testing.T, store *LocalStore, remote storage.ObjectStore, at time.Time) Result {
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

	state := readPublishedStateFile(t, local, reg.ArchiveSessionID)
	if state.LastPublished == nil || !state.LastPublished.SameAsBundle || len(state.LastPublished.Bundle.NativeRecords) != 0 {
		t.Fatalf("the published bundle is stored twice: %#v", state.LastPublished)
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
	if status != CacheStatusRateLimited || len(candidate.NativeRecords) != len(published.NativeRecords)+1 {
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
	legacy := publishedState{Bundle: bundle, PublishedAt: at, Status: CacheStatusPublished, LastPublished: &publishedSnapshot{Bundle: bundle, PublishedAt: at}}
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
func settledSession(t *testing.T, local *LocalStore, content string) archive.SessionRegistration {
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
	check := func(t *testing.T, local *LocalStore, reg archive.SessionRegistration, o Options) bool {
		t.Helper()
		unchanged, err := unchangedSinceLastScan(local, reg, o)
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
		if err := local.SavePending(reg.ArchiveSessionID, PendingPublication{SourceKey: "k", MetadataKey: "m", SourceSHA256: "s", SourceBytes: []byte{1}, MetadataBytes: []byte{1}}); err != nil {
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
		if _, err := blockSession(local, reg.ArchiveSessionID, Request{}, BlockedReasonTranscriptRewritten, nil); err != nil {
			t.Fatal(err)
		}
		if check(t, local, reg, opts) {
			t.Fatal("a recorded gap was skipped")
		}
	})
	t.Run("subagent", func(t *testing.T) {
		local := newTestStore(t)
		reg := settledSession(t, local, codexTranscript)
		reg.ParentSessionID = "parent"
		if check(t, local, reg, opts) {
			t.Fatal("a subagent was skipped")
		}
	})
	t.Run("cursor text is never trusted to a stat", func(t *testing.T) {
		local := newTestStore(t)
		reg := settledSession(t, local, codexTranscript)
		signature, _, _ := local.loadScanSignature(reg.ArchiveSessionID)
		signature.SourceFormat = cursorTextSourceFormat
		if err := local.saveScanSignature(reg.ArchiveSessionID, signature); err != nil {
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
	if reason, blocked, _ := local.LoadBlocked(reg.ArchiveSessionID); !blocked || reason != BlockedReasonTranscriptRewritten {
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
// and no reads of the published cache at all. The documented number comes
// from the plain run; under the race detector the same invariants are checked
// on 20 sessions, because setup alone would otherwise take minutes.
func TestUnchangedSessionsCostNoWritesAndStayFast(t *testing.T) {
	sessions, size := 300, 400*1024
	if raceEnabled {
		sessions = 20
	}
	if testing.Short() {
		t.Skip("builds and publishes up to 120 MB of synthetic transcripts")
	}
	home := t.TempDir()
	local, err := NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	remote := storage.NewMemoryStore()
	dir := t.TempDir()
	content := largeCodexTranscript(size)
	for i := 0; i < sessions; i++ {
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
	// signatures so every session is read, filtered, compared, and journaled.
	signatures, _ := filepath.Glob(filepath.Join(home, "scan-signatures", "*.json"))
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
	if changed < sessions {
		t.Fatalf("the snapshot saw only %d changed paths after a full pass over %d sessions", changed, sessions)
	}

	t.Logf("%d unchanged sessions of %d KB: short-circuit pass %s (%.2f ms/session); full re-read pass %s", sessions, size/1024, elapsed, float64(elapsed.Microseconds())/1000/float64(sessions), fullScan)
	if elapsed >= time.Second {
		t.Fatalf("an unchanged pass took %s, want well under one second", elapsed)
	}
}

// The on-disk published cache after one publication, for the ledger: the
// shared-copy marker roughly halves it.
func TestPublishedCacheSizeIsAboutOneBundle(t *testing.T) {
	local := newTestStore(t)
	reg := settledSession(t, local, largeCodexTranscript(64*1024))
	raw, err := os.ReadFile(local.publishedPath(reg.ArchiveSessionID))
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
