package collector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// A well-formed file of an unexpected shape, as a newer version might leave
// behind before a downgrade, is reported but never moved aside.
func TestWrongShapeStateFileIsReportedNotQuarantined(t *testing.T) {
	local := newTestStore(t)
	if err := os.WriteFile(local.registrationPath("newer"), []byte(`{"archive_session_id":["not","a","string"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), local, storage.NewMemoryStore(), Options{MachineID: "m"})
	if err != nil || result.Errors["newer"] == nil || errors.Is(result.Errors["newer"], ErrQuarantined) {
		t.Fatalf("%#v %v %v", result, err, result.Errors)
	}
	if _, err := os.Stat(local.registrationPath("newer")); err != nil {
		t.Fatalf("the file was moved: %v", err)
	}
}

// A file corrupted again after an earlier quarantine is moved aside next to
// the first copy, never over it.
func TestSecondQuarantineKeepsTheFirst(t *testing.T) {
	local := newTestStore(t)
	for pass := 0; pass < 2; pass++ {
		corruptFile(t, local.requestPath("orphan"))
		if _, err := Run(context.Background(), local, storage.NewMemoryStore(), Options{MachineID: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	if files := local.quarantinedFiles(); len(files) != 2 {
		t.Fatalf("quarantined = %v, want both copies", files)
	}
}

// A registration that cannot be read at all (here: no permission) is
// counted as outstanding work.
func TestUnreadableRegistrationCountsAsPending(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript+"\n"))); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(local.registrationPath("session-1"), 0); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), local, storage.NewMemoryStore(), Options{MachineID: "m"})
	if err != nil || result.Errors["session-1"] == nil {
		t.Fatalf("%#v %v %v", result, err, result.Errors)
	}
	if status, err := local.LoadStatus(); err != nil || status.PendingCount != 1 {
		t.Fatalf("status = %#v %v", status, err)
	}
}

// State written before the source reference was recorded carries it in
// cached metadata of any metadata schema version.
func TestOlderStateSourceReferenceIgnoresMetadataSchemaVersion(t *testing.T) {
	ref := archive.SourceReference{Key: "sessions/codex/s/source.x.jsonl.gz", SHA256: strings.Repeat("ab", 32), CompressedBytes: 10}
	metadata, err := json.Marshal(map[string]any{"schema_version": 99, "source_bundle": ref})
	if err != nil {
		t.Fatal(err)
	}
	state := publishedState{Status: CacheStatusPublished, MetadataBytes: metadata}
	if got, found := state.lastPublishedSource(); !found || got != ref {
		t.Fatalf("source = %#v %v", got, found)
	}
	state.MetadataBytes, _ = json.Marshal(map[string]any{"source_bundle": map[string]any{"key": ref.Key, "sha256": "not-a-digest"}})
	if _, found := state.lastPublishedSource(); found {
		t.Fatal("accepted a reference without a SHA-256 digest")
	}
}

// ForgetSession must not go ahead while the session's own subagent candidate
// cannot be read: the candidate would register the session again later.
func TestForgetSessionRefusesWhileItsCandidateIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	store := newTestStore(t)
	if err := store.SaveRegistration(registration(t, "/unused")); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	candidate := SubagentCandidate{ArchiveSessionID: "session-1", NativeSessionID: "native-1", ParentArchiveSessionID: "parent", ParentNativeSessionID: "native-parent", ProjectID: "p", ProjectRoot: "/p", Harness: archive.Harness{Name: "claude"}, AgentID: "agent", TranscriptPath: "/unused", ObservedAt: at}
	if err := store.SaveSubagentCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(store.subagentCandidatePath("session-1"), 0); err != nil {
		t.Fatal(err)
	}
	if err := store.ForgetSession("session-1", "native-1"); err == nil {
		t.Fatal("forgot a session whose candidate could not be read")
	}
	if _, found, err := store.LoadRegistration("session-1"); err != nil || !found {
		t.Fatalf("registration = %v %v", found, err)
	}
}

// uploadedByAnotherBuild makes session-1's last publication one whose source
// bytes this build does not reproduce from the cached bundle, as after a
// compressor change: the recorded reference, and the cached metadata, point
// at an object with other bytes. It returns that reference.
func uploadedByAnotherBuild(t *testing.T, local *LocalStore, store storage.ObjectStore) archive.SourceReference {
	t.Helper()
	data := []byte("the same bundle, compressed by another build")
	sum := storage.SHA256Hex(data)
	ref := archive.SourceReference{Key: "sessions/codex/session-1/source." + sum + ".jsonl.gz", SHA256: sum, CompressedBytes: len(data)}
	if err := store.Put(context.Background(), ref.Key, data); err != nil {
		t.Fatal(err)
	}
	editPublishedState(t, local, "session-1", func(state map[string]any) {
		state["last_published"].(map[string]any)["source"] = ref
		raw, err := base64.StdEncoding.DecodeString(state["metadata_bytes"].(string))
		if err != nil {
			t.Fatal(err)
		}
		var metadata map[string]any
		if err := json.Unmarshal(raw, &metadata); err != nil {
			t.Fatal(err)
		}
		metadata["source_bundle"] = ref
		if state["metadata_bytes"], err = json.Marshal(metadata); err != nil {
			t.Fatal(err)
		}
	})
	return ref
}

// A session that no longer changes (its transcript is gone) still gets
// current-parser metadata when this build cannot reproduce its source's
// bytes: the metadata-only publication checks the recorded source in storage
// instead of rebuilding it.
func TestParserUpgradePublishesAgainstRecordedSourceItCannotRebuild(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	path := writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript+"\n")
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	opts := Options{MachineID: "m", ParserVersion: "one", Now: func() time.Time { return now }}
	if result, err := Run(context.Background(), local, store, opts); err != nil || len(result.Published) != 1 {
		t.Fatalf("%#v %v %v", result, err, result.Errors)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	first := uploadedByAnotherBuild(t, local, store)
	now = now.Add(time.Hour)
	opts.ParserVersion = "two"
	result, err := Run(context.Background(), local, store, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("%#v %v %v", result, err, result.Errors)
	}
	metadata := fetchMetadata(t, store, "codex", "session-1")
	if metadata.Parser.Version != "two" || metadata.SourceBundle != first {
		t.Fatalf("metadata parser %q source %#v, want parser two over %#v", metadata.Parser.Version, metadata.SourceBundle, first)
	}
	if superseded, _ := local.LoadSuperseded("session-1"); len(superseded) != 0 {
		t.Fatalf("a metadata-only publication superseded %v", superseded)
	}
}

// If the recorded source is not in storage, the metadata-only publication
// cannot succeed: it is reported and dropped, so it never wedges the session.
func TestMetadataOnlyPublicationOverMissingSourceIsDropped(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	path := writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript+"\n")
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	opts := Options{MachineID: "m", ParserVersion: "one", Now: func() time.Time { return now }, Retry: storage.RetryPolicy{MaxAttempts: 1}}
	if _, err := Run(context.Background(), local, store, opts); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	recorded := uploadedByAnotherBuild(t, local, store)
	if err := store.Delete(context.Background(), recorded.Key); err != nil {
		t.Fatal(err)
	}
	opts.ParserVersion = "two"
	result, err := Run(context.Background(), local, store, opts)
	if err != nil || !errors.Is(result.Errors["session-1"], storage.ErrNotFound) {
		t.Fatalf("%#v %v %v", result, err, result.Errors)
	}
	if pending, err := local.HasPending("session-1"); err != nil || pending {
		t.Fatalf("the unpublishable publication is still pending: %v %v", pending, err)
	}
	if got := fetchMetadata(t, store, "codex", "session-1").Parser.Version; got != "one" {
		t.Fatalf("metadata was replaced: parser %q", got)
	}
}

// blockingPutStore holds every Put until the pass's context ends.
type blockingPutStore struct {
	*storage.MemoryStore
	started chan struct{}
}

func (s *blockingPutStore) Put(ctx context.Context, key string, data []byte) error {
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

// A session cut off by the pass's deadline mid-upload is not a failure: its
// publication stays pending for the next pass.
func TestSessionCutOffByPassDeadlineIsNotAFailure(t *testing.T) {
	local := newTestStore(t)
	store := &blockingPutStore{MemoryStore: storage.NewMemoryStore(), started: make(chan struct{}, 1)}
	if err := local.SaveRegistration(registration(t, writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript+"\n"))); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-store.started
		cancel()
	}()
	result, err := Run(ctx, local, store, Options{MachineID: "m", Retry: storage.RetryPolicy{MaxAttempts: 1}})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("%#v %v %v", result, err, result.Errors)
	}
	if pending, err := local.HasPending("session-1"); err != nil || !pending {
		t.Fatalf("publication not kept for the next pass: %v %v", pending, err)
	}
	if status, err := local.LoadStatus(); err != nil || status.LastError != "" || status.PendingCount != 1 {
		t.Fatalf("status = %#v %v", status, err)
	}
}
