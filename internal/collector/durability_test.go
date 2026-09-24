package collector

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

const grownTranscript = codexTranscript + "\n" + `{"type":"response_item","id":"m2","payload":{"type":"message","role":"assistant","content":"more"}}` + "\n"

// editPublishedState rewrites a session's published state as raw JSON, to
// stand in for state an older build wrote.
func editPublishedState(t *testing.T, local *LocalStore, id string, edit func(state map[string]any)) {
	t.Helper()
	data, err := os.ReadFile(local.publishedPath(id))
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	edit(state)
	if data, err = json.Marshal(state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local.publishedPath(id), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// olderSourceSchema marks the cached bundle as written under source schema 1,
// which this build can no longer serialize: what every session's cache looks
// like right after a SourceSchemaVersion bump.
func olderSourceSchema(state map[string]any) {
	state["bundle"].(map[string]any)["schema_version"] = 1
}

// withoutRecordedSource drops publishedSnapshot.Source, as state written
// before it existed has none.
func withoutRecordedSource(state map[string]any) {
	if last, ok := state["last_published"].(map[string]any); ok {
		delete(last, "source")
	}
}

// publishThenGrow publishes session-1 at t0, then grows its transcript and
// queues a stop request, and returns the first publication's source key.
func publishThenGrow(t *testing.T, local *LocalStore, store storage.ObjectStore, t0 time.Time) (path, firstKey string) {
	t.Helper()
	path = writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript+"\n")
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	if result, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return t0 }}); err != nil || len(result.Published) != 1 {
		t.Fatalf("first publication: %#v %v", result, err)
	}
	firstKey = fetchMetadata(t, store, "codex", "session-1").SourceBundle.Key
	writeTranscript(t, filepath.Dir(path), "codex.jsonl", grownTranscript)
	if err := local.SaveRequest("session-1", "stop", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return path, firstKey
}

func assertRepublishedSuperseding(t *testing.T, local *LocalStore, store storage.ObjectStore, now time.Time, wantSuperseded []string) {
	t.Helper()
	result, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return now }})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("republish: %#v %v", result, err)
	}
	if pending, err := local.HasPending("session-1"); err != nil || pending {
		t.Fatalf("publication left pending: %v %v", pending, err)
	}
	superseded, err := local.LoadSuperseded("session-1")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, s := range superseded {
		keys = append(keys, s.Key)
	}
	if strings.Join(keys, ",") != strings.Join(wantSuperseded, ",") {
		t.Fatalf("superseded = %v, want %v", keys, wantSuperseded)
	}
	// The source key the new publication recorded is the one its metadata
	// names, so the publication after it supersedes the right object.
	current := fetchMetadata(t, store, "codex", "session-1").SourceBundle
	if source, found, err := local.loadLastPublishedSource("session-1"); err != nil || !found || source != current {
		t.Fatalf("recorded source = %#v %v %v, want %#v", source, found, err, current)
	}
}

// A source schema bump leaves every cached bundle at a schema this build
// cannot serialize. Publishing must still complete and record the object it
// really replaced, rather than failing after the upload on every pass.
func TestPublishAfterSourceSchemaBumpSupersedesUploadedKey(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	_, firstKey := publishThenGrow(t, local, store, t0)
	editPublishedState(t, local, "session-1", olderSourceSchema)
	assertRepublishedSuperseding(t, local, store, t0.Add(time.Hour), []string{firstKey})
}

// State written before the source reference was recorded falls back to the
// cached metadata, which names exactly the uploaded object.
func TestPublishWithOlderStateReadsSupersededKeyFromCachedMetadata(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	_, firstKey := publishThenGrow(t, local, store, t0)
	editPublishedState(t, local, "session-1", func(state map[string]any) {
		olderSourceSchema(state)
		withoutRecordedSource(state)
	})
	assertRepublishedSuperseding(t, local, store, t0.Add(time.Hour), []string{firstKey})
}

// With neither record, the previous key is unknown: the publication still
// completes, and nothing is guessed into the superseded ledger.
func TestPublishWithUnknownPreviousSourceStillCompletes(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	publishThenGrow(t, local, store, t0)
	editPublishedState(t, local, "session-1", func(state map[string]any) {
		olderSourceSchema(state)
		withoutRecordedSource(state)
		delete(state, "metadata_bytes")
	})
	assertRepublishedSuperseding(t, local, store, t0.Add(time.Hour), nil)
}

// A parser upgrade over a cached bundle this build cannot reproduce skips the
// metadata-only refresh instead of failing the session; normal capture goes
// on and publishes current metadata.
func TestParserUpgradeOverUnreproducibleBundleDoesNotFailSession(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	path := writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript+"\n")
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	opts := Options{MachineID: "m", ParserVersion: "one", Now: func() time.Time { return now }}
	if result, err := Run(context.Background(), local, store, opts); err != nil || len(result.Published) != 1 {
		t.Fatalf("%#v %v", result, err)
	}
	editPublishedState(t, local, "session-1", olderSourceSchema)
	now = now.Add(time.Hour)
	opts.ParserVersion = "two"
	result, err := Run(context.Background(), local, store, opts)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	if got := fetchMetadata(t, store, "codex", "session-1").Parser.Version; got != "two" {
		t.Fatalf("published parser = %q", got)
	}
}

// A publication stamped in the future by a wrong clock must not hold back
// the next one until that date: it defers by at most one interval.
func TestFutureLastPublicationDefersByAtMostOneInterval(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	path := writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript+"\n")
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	future := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return future }}); err != nil {
		t.Fatal(err)
	}
	// The clock is corrected and the transcript grows, with no hook asking
	// for a flush.
	writeTranscript(t, filepath.Dir(path), "codex.jsonl", grownTranscript)
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	opts := Options{MachineID: "m", Now: func() time.Time { return now }}
	if result, err := Run(context.Background(), local, store, opts); err != nil || len(result.Published) != 0 {
		t.Fatalf("expected one interval's deferral: %#v %v", result, err)
	}
	now = now.Add(opts.minUploadInterval())
	if result, err := Run(context.Background(), local, store, opts); err != nil || len(result.Published) != 1 {
		t.Fatalf("still deferred after one interval: %#v %v", result, err)
	}
}

// A pending publication already carrying a far-future ReadyAt is capped at
// one interval from now, durably, so it publishes once that has passed.
func TestPendingReadyAtInTheFutureIsCapped(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	path := writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript+"\n")
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	opts := Options{MachineID: "m", Now: func() time.Time { return now }}
	if _, err := Run(context.Background(), local, store, opts); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, filepath.Dir(path), "codex.jsonl", grownTranscript)
	now = now.Add(time.Minute)
	if result, err := Run(context.Background(), local, store, opts); err != nil || len(result.Published) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	pending, found, err := local.LoadPending("session-1")
	if err != nil || !found {
		t.Fatalf("no rate-limited publication: %v %v", found, err)
	}
	pending.ReadyAt = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := local.SavePending("session-1", pending); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if result, err := Run(context.Background(), local, store, opts); err != nil || len(result.Published) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	now = now.Add(opts.minUploadInterval())
	if result, err := Run(context.Background(), local, store, opts); err != nil || len(result.Published) != 1 {
		t.Fatalf("far-future ReadyAt still holds the publication: %#v %v", result, err)
	}
}

func corruptFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(`{"archive_session_id":"trunc`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// One truncated state file fails only its own session: it is moved aside and
// reported, every other session is published, and the pass completes.
func TestCorruptStateFilesAreQuarantinedPerSession(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	dir := t.TempDir()
	if err := local.SaveRegistration(registration(t, writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"))); err != nil {
		t.Fatal(err)
	}
	other := registration(t, writeTranscript(t, dir, "other.jsonl", codexTranscript+"\n"))
	other.ArchiveSessionID, other.NativeSessionID = "session-2", "native-2"
	if err := local.SaveRegistration(other); err != nil {
		t.Fatal(err)
	}
	// session-2's request, an unrelated request, a registration, and a
	// subagent candidate, each truncated.
	corruptFile(t, local.requestPath("session-2"))
	corruptFile(t, local.requestPath("orphan"))
	corruptFile(t, local.registrationPath("broken"))
	corruptFile(t, local.subagentCandidatePath("child"))

	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	result, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("one corrupt file failed the whole pass: %v", err)
	}
	if strings.Join(result.Published, ",") != "session-1,session-2" {
		t.Fatalf("published = %v", result.Published)
	}
	for _, id := range []string{"session-2", "orphan", "broken", "child"} {
		if !errors.Is(result.Errors[id], ErrQuarantined) {
			t.Fatalf("%s: error = %v, want ErrQuarantined", id, result.Errors[id])
		}
	}
	want := []string{"registrations/broken.json.corrupt", "requests/orphan.json.corrupt", "requests/session-2.json.corrupt", "subagent-candidates/child.json.corrupt"}
	for _, rel := range want {
		if _, err := os.Stat(filepath.Join(local.home, rel)); err != nil {
			t.Fatalf("not quarantined: %v", err)
		}
	}
	status, err := local.LoadStatus()
	if err != nil || strings.Join(status.QuarantinedFiles, ",") != strings.Join(want, ",") {
		t.Fatalf("status quarantined = %v %v", status.QuarantinedFiles, err)
	}
	// Quarantined once: the next pass is clean, and hooks can write again.
	if err := local.SaveRequest("session-2", "stop", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if result, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return now }}); err != nil || len(result.Errors) != 0 {
		t.Fatalf("second pass: %#v %v", result, err)
	}
}

// A request that cannot be read for any other reason stays where it is and
// holds its session back, without failing the pass.
func TestUnreadableRequestHoldsOnlyItsSession(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	if err := local.SaveRegistration(registration(t, writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript+"\n"))); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := local.SaveRequest("session-1", "stop", now); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(local.requestPath("session-1"), 0); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return now }})
	if err != nil || result.Errors["session-1"] == nil || errors.Is(result.Errors["session-1"], ErrQuarantined) || len(result.Published) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	if _, err := os.Stat(local.requestPath("session-1")); err != nil {
		t.Fatalf("unreadable request was moved: %v", err)
	}
}

// A local failure partway through one session is that session's error; the
// pass goes on to the others and still records its status.
func TestMidPassLocalFailureIsPerSession(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	dir := t.TempDir()
	if err := local.SaveRegistration(registration(t, writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"))); err != nil {
		t.Fatal(err)
	}
	stuck := registration(t, writeTranscript(t, dir, "stuck.jsonl", codexTranscript+"\n"))
	stuck.ArchiveSessionID, stuck.NativeSessionID = "session-0", "native-0"
	if err := local.SaveRegistration(stuck); err != nil {
		t.Fatal(err)
	}
	// The scan journal cannot be written for session-0: a non-empty
	// directory occupies its path.
	if err := os.MkdirAll(filepath.Join(local.home, "pending-scans", "session-0.json", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	result, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("one session's local failure ended the pass: %v", err)
	}
	if result.Errors["session-0"] == nil || strings.Join(result.Published, ",") != "session-1" {
		t.Fatalf("%#v", result)
	}
	if status, err := local.LoadStatus(); err != nil || !status.LastScanAt.Equal(now) || status.PendingCount != 1 {
		t.Fatalf("status = %#v %v", status, err)
	}
}

// A pass whose context has expired stops before the next session, leaving
// its work pending, instead of failing every remaining session.
func TestExpiredPassContextLeavesSessionsPending(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	if err := local.SaveRegistration(registration(t, writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript+"\n"))); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := local.SaveRequest("session-1", "stop", now); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := Run(ctx, local, store, Options{MachineID: "m", Now: func() time.Time { return now }})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	if status, err := local.LoadStatus(); err != nil || status.PendingCount != 1 {
		t.Fatalf("status = %#v %v", status, err)
	}
}

// A hook-supplied transcript path naming a FIFO fails its session at once
// instead of blocking the pass on an open that waits for a writer.
func TestFIFOTranscriptFailsWithoutBlockingThePass(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	dir := t.TempDir()
	if err := local.SaveRegistration(registration(t, writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"))); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	blocked := registration(t, fifo)
	blocked.ArchiveSessionID, blocked.NativeSessionID = "session-0", "native-0"
	if err := local.SaveRegistration(blocked); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return now }})
		done <- outcome{result, err}
	}()
	select {
	case got := <-done:
		if got.err != nil || !errors.Is(got.result.Errors["session-0"], errNotRegularFile) || strings.Join(got.result.Published, ",") != "session-1" {
			t.Fatalf("%#v %v", got.result, got.err)
		}
	case <-time.After(10 * time.Second):
		// Unblock the stuck open so the goroutine can end.
		if f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
		t.Fatal("pass blocked opening a FIFO transcript")
	}
}

func TestOpenRegularFileRefusesNonRegularPaths(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, fifo, "/dev/null"} {
		if f, err := openRegularFile(path); !errors.Is(err, errNotRegularFile) {
			if f != nil {
				f.Close()
			}
			t.Fatalf("%s: err = %v", path, err)
		}
	}
	regular := writeTranscript(t, dir, "t.jsonl", codexTranscript)
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	f, err := openRegularFile(link)
	if err != nil {
		t.Fatalf("symlink to a regular file: %v", err)
	}
	f.Close()
}

func TestForgetSessionRemovesSubagentCandidateLock(t *testing.T) {
	store := newTestStore(t)
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	candidate := SubagentCandidate{ArchiveSessionID: "session-1", NativeSessionID: "native-1", ParentArchiveSessionID: "parent", ParentNativeSessionID: "native-parent", ProjectID: "p", ProjectRoot: "/p", Harness: archive.Harness{Name: "claude"}, AgentID: "agent", TranscriptPath: "/unused", ObservedAt: at}
	if err := store.SaveSubagentCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(store.home, subagentLockName("session-1"))
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("candidate lock was not created: %v", err)
	}
	if err := store.ForgetSession("session-1", "native-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("candidate lock leaked after ForgetSession: %v", err)
	}
}

// A pass clears the atomic-write temporaries a crashed writer left, and only
// those: one young enough to belong to a write in progress stays.
func TestRunRemovesStaleWriteTemporaries(t *testing.T) {
	local := newTestStore(t)
	stale := filepath.Join(local.home, "requests", ".pending-stale")
	fresh := filepath.Join(local.home, "published", ".pending-fresh")
	nested := filepath.Join(local.home, "sessions", "session-1", ".pending-stale")
	for _, path := range []string{stale, fresh, nested} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleTempAge)
	for _, path := range []string{stale, nested} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Run(context.Background(), local, storage.NewMemoryStore(), Options{MachineID: "m"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{stale, nested} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale temporary %s survived: %v", path, err)
		}
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh temporary was removed: %v", err)
	}
}
