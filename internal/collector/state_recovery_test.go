package collector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// A well-formed file of an unexpected shape, as a newer version might leave
// behind before a downgrade, is reported but never moved aside.
func TestWrongShapeStateFileIsReportedNotQuarantined(t *testing.T) {
	local := newTestStore(t)
	if err := os.WriteFile(registrationPath(local, "newer"), []byte(`{"archive_session_id":["not","a","string"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), local, storage.NewMemoryStore(), Options{MachineID: "m"})
	if err != nil || result.Errors["newer"] == nil || errors.Is(result.Errors["newer"], state.ErrQuarantined) {
		t.Fatalf("%#v %v %v", result, err, result.Errors)
	}
	if _, err := os.Stat(registrationPath(local, "newer")); err != nil {
		t.Fatalf("the file was moved: %v", err)
	}
}

// A file corrupted again after an earlier quarantine is moved aside next to
// the first copy, never over it.
func TestSecondQuarantineKeepsTheFirst(t *testing.T) {
	local := newTestStore(t)
	for range 2 {
		corruptFile(t, requestPath(local, "orphan"))
		if _, err := Run(context.Background(), local, storage.NewMemoryStore(), Options{MachineID: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	if files := local.QuarantinedFiles(); len(files) != 2 {
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
	if err := os.Chmod(registrationPath(local, "session-1"), 0); err != nil {
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

// uploadedByAnotherBuild makes session-1's last publication one whose source
// bytes this build does not reproduce from the cached bundle, as after a
// compressor change: the recorded reference, and the cached metadata, point
// at an object with other bytes. It returns that reference.
func uploadedByAnotherBuild(t *testing.T, local *state.Store, store storage.ObjectStore) archive.SourceReference {
	t.Helper()
	data := []byte("the same bundle, compressed by another build")
	sum := storage.SHA256Hex(data)
	ref := archive.SourceReference{Key: "sessions/codex/session-1/source." + sum + ".jsonl.gz", SHA256: sum, CompressedBytes: len(data)}
	if err := store.Put(context.Background(), ref.Key, data); err != nil {
		t.Fatal(err)
	}
	editPublishedState(t, local, func(state map[string]any) {
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
// current-parser metadata when this build builds its retained bundle to
// other bytes than the uploaded source (a compressor change): the rebuilt
// source is published and supersedes the old one, whether or not the old one
// is still in storage. The pass after that has nothing left to do.
func TestParserUpgradeRepublishesSourceThisBuildBuildsDifferently(t *testing.T) {
	for _, oldSourceGone := range []bool{false, true} {
		t.Run(map[bool]string{false: "old source present", true: "old source missing"}[oldSourceGone], func(t *testing.T) {
			local := newTestStore(t)
			store := storage.NewMemoryStore()
			path := writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript+"\n")
			if err := local.SaveRegistration(registration(t, path)); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
			opts := Options{MachineID: "m", ParserVersion: "one", Now: func() time.Time { return now }, Retry: storage.RetryPolicy{MaxAttempts: 1}}
			if result, err := Run(context.Background(), local, store, opts); err != nil || len(result.Published) != 1 {
				t.Fatalf("%#v %v %v", result, err, result.Errors)
			}
			rebuilt := fetchMetadata(t, store, "codex", "session-1").SourceBundle
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			other := uploadedByAnotherBuild(t, local, store)
			if oldSourceGone {
				if err := store.Delete(context.Background(), other.Key); err != nil {
					t.Fatal(err)
				}
			}
			opts.ParserVersion = "two"
			for pass := 1; pass <= 2; pass++ {
				now = now.Add(time.Hour)
				result, err := Run(context.Background(), local, store, opts)
				if err != nil || len(result.Errors) != 0 || len(result.Published) != 2-pass {
					t.Fatalf("pass %d: %#v %v %v", pass, result, err, result.Errors)
				}
			}
			metadata := fetchMetadata(t, store, "codex", "session-1")
			if metadata.Parser.Version != "two" || metadata.SourceBundle != rebuilt {
				t.Fatalf("metadata parser %q source %#v, want parser two over %#v", metadata.Parser.Version, metadata.SourceBundle, rebuilt)
			}
			if superseded, _ := local.LoadSuperseded("session-1"); len(superseded) != 1 || superseded[0].Key != other.Key {
				t.Fatalf("superseded = %v, want the other build's source", superseded)
			}
		})
	}
}

// sourceReadCounter counts reads (Get or Stat) of one object.
type sourceReadCounter struct {
	*storage.MemoryStore
	key   string
	reads int
}

func (s *sourceReadCounter) Get(ctx context.Context, key string) ([]byte, error) {
	if key == s.key {
		s.reads++
	}
	return s.MemoryStore.Get(ctx, key)
}

func (s *sourceReadCounter) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if key == s.key {
		s.reads++
	}
	return s.MemoryStore.Stat(ctx, key)
}

// A metadata-only publication that carries only the recorded source reference
// (this build cannot build the retained bundle) cannot succeed when that
// source is gone from storage or differs from its record. It is reported
// once, dropped so it never wedges the session, and recorded as a refresh
// this parser cannot do: later passes neither retry it nor read the source
// again, and status counts it.
func TestUnverifiableRecordedSourceIsReportedOnceAndNotRetried(t *testing.T) {
	for _, damage := range []string{"missing", "different"} {
		t.Run(damage, func(t *testing.T) {
			local := newTestStore(t)
			memory := storage.NewMemoryStore()
			path := writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript+"\n")
			if err := local.SaveRegistration(registration(t, path)); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
			opts := Options{MachineID: "m", ParserVersion: "one", Now: func() time.Time { return now }, Retry: storage.RetryPolicy{MaxAttempts: 1}}
			if _, err := Run(context.Background(), local, memory, opts); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			editPublishedState(t, local, olderSourceSchema)
			recorded, _, err := local.LoadLastPublishedSource("session-1")
			if err != nil {
				t.Fatal(err)
			}
			bundle, _, _, err := local.LoadLastPublished("session-1")
			if err != nil {
				t.Fatal(err)
			}
			metadataKey, _ := archive.MetadataObjectKey("codex", "session-1")
			if err := local.SavePending("session-1", state.PendingPublication{
				MetadataOnly: true, Bundle: bundle, SourceKey: recorded.Key, SourceSHA256: recorded.SHA256, SourceSize: recorded.CompressedBytes,
				MetadataKey: metadataKey, MetadataBytes: []byte(`{}`), ReadyAt: now,
			}); err != nil {
				t.Fatal(err)
			}
			wantErr := storage.ErrNotFound
			if damage == "missing" {
				if err := memory.Delete(context.Background(), recorded.Key); err != nil {
					t.Fatal(err)
				}
			} else {
				wantErr = storage.ErrChecksumMismatch
				if err := memory.Put(context.Background(), recorded.Key, []byte("other bytes")); err != nil {
					t.Fatal(err)
				}
			}
			store := &sourceReadCounter{MemoryStore: memory, key: recorded.Key}
			opts.ParserVersion = "two"
			for pass := 1; pass <= 3; pass++ {
				now = now.Add(time.Hour)
				before := store.reads
				result, err := Run(context.Background(), local, store, opts)
				if err != nil {
					t.Fatal(err)
				}
				if pass == 1 && !errors.Is(result.Errors["session-1"], wantErr) {
					t.Fatalf("pass 1: errors %v, want %v", result.Errors, wantErr)
				}
				if pass > 1 && (len(result.Errors) != 0 || store.reads != before) {
					t.Fatalf("pass %d: errors %v, %d source reads: the refresh was retried", pass, result.Errors, store.reads-before)
				}
				if pending, err := local.HasPending("session-1"); err != nil || pending {
					t.Fatalf("pass %d: the unpublishable publication is still pending: %v %v", pass, pending, err)
				}
				if status, err := local.LoadStatus(); err != nil || status.UnrefreshableSummaries != 1 || (pass > 1 && status.LastError != "") {
					t.Fatalf("pass %d: status %#v %v", pass, status, err)
				}
			}
			if got := fetchMetadata(t, memory, "codex", "session-1").Parser.Version; got != "one" {
				t.Fatalf("metadata was replaced: parser %q", got)
			}
		})
	}
}

// Metadata this build cannot derive from the retained bundle at all is
// recorded rather than retried on every pass, counted in status, and the
// record is dropped once the session publishes again.
func TestUnderivableMetadataIsRecordedUntilTheNextPublication(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n")
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	opts := Options{MachineID: "m", ParserVersion: "one", Now: func() time.Time { return now }}
	if _, err := Run(context.Background(), local, store, opts); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	editPublishedState(t, local, olderSourceSchema)
	opts.ParserVersion = "two"
	for pass := 1; pass <= 2; pass++ {
		now = now.Add(time.Hour)
		if result, err := Run(context.Background(), local, store, opts); err != nil || len(result.Errors) != 0 {
			t.Fatalf("pass %d: %#v %v", pass, result, err)
		}
		if status, err := local.LoadStatus(); err != nil || status.UnrefreshableSummaries != 1 {
			t.Fatalf("pass %d: status %#v %v", pass, status, err)
		}
	}
	if skip, found, _ := local.LoadRefreshSkip("session-1"); !found || skip.Reason != state.RefreshSkipUnderivable || skip.ParserVersion != "two" {
		t.Fatalf("refresh skip = %#v %v", skip, found)
	}
	// The transcript comes back with more: a new publication, which clears it.
	writeTranscript(t, dir, "codex.jsonl", grownTranscript)
	now = now.Add(time.Hour)
	if result, err := Run(context.Background(), local, store, opts); err != nil || len(result.Published) != 1 {
		t.Fatalf("%#v %v", result, err)
	}
	if status, err := local.LoadStatus(); err != nil || status.UnrefreshableSummaries != 0 {
		t.Fatalf("status %#v %v", status, err)
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
