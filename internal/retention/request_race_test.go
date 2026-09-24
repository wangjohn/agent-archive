package retention

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// hookDuringDeleteStore runs a hook on the first remote delete: the moment
// retention has already decided a session expired, from a snapshot of the
// pending requests, and has not yet forgotten it locally.
type hookDuringDeleteStore struct {
	storage.ObjectStore
	once sync.Once
	hook func()
}

func (h *hookDuringDeleteStore) Delete(ctx context.Context, key string) error {
	h.once.Do(h.hook)
	return h.ObjectStore.Delete(ctx, key)
}

func finalResponse(t *testing.T, at time.Time) archive.SupplementalEvidence {
	t.Helper()
	filtered, _, err := archive.FilterSupplementalEvidence([]archive.SupplementalEvidence{{
		Kind: archive.EvidenceKindFinalResponse, ObservedAt: at, Provenance: "hook:codex:stop",
		Payload: map[string]any{"event_name": "Stop", "turn_id": "late-turn", "text": "a final answer written mid-expiry"},
	}})
	if err != nil || len(filtered) != 1 {
		t.Fatalf("filter: %v", err)
	}
	return filtered[0]
}

// A hook writes a request after retention snapshotted the pending requests
// and deleted the session's objects, but before it forgot the session. The
// request and its evidence must survive, and the next collector pass must
// publish that evidence rather than lose it.
func TestHookRequestWrittenMidExpiryIsKeptAndPublished(t *testing.T) {
	dir := t.TempDir()
	local := newTestStore(t)
	memory := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := local.SaveRegistration(registration("s1", writeTranscript(t, dir, "s1.jsonl", codexTranscript))); err != nil {
		t.Fatal(err)
	}
	collect(t, local, memory, t0)

	expiry := t0.Add(91 * 24 * time.Hour)
	var hookErr error
	store := &hookDuringDeleteStore{ObjectStore: memory, hook: func() {
		hookErr = local.SaveRequest("s1", "stop", expiry, finalResponse(t, expiry))
	}}
	result := sweep(t, local, store, expiry, Options{})
	if hookErr != nil {
		t.Fatalf("the hook's request write failed: %v", hookErr)
	}
	if len(result.Errors) != 0 || len(result.DeletedSessions) != 0 {
		t.Fatalf("the session was expired over a request written mid-expiry: %#v", result)
	}
	if registered(t, local) != 1 {
		t.Fatal("the registration was forgotten")
	}
	requests, err := local.LoadRequests()
	if err != nil || len(requests) != 1 || len(requests[0].HookEvidence) != 1 {
		t.Fatalf("the hook's request was lost: %#v %v", requests, err)
	}

	// The collector republishes the whole session with that evidence, so the
	// remote deletion that already happened loses nothing.
	if result := collect(t, local, memory, expiry); len(result.Published) != 1 {
		t.Fatalf("the kept request was not published: %#v", result)
	}
	meta := fetchMetadata(t, memory, "codex", "s1")
	if !meta.CapturedAt.Equal(expiry) {
		t.Fatalf("captured_at=%s, want the republication at %s", meta.CapturedAt, expiry)
	}
	bundle, err := reader.LoadSource(context.Background(), memory, meta, reader.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range bundle.SupplementalEvidence {
		if item.Kind == archive.EvidenceKindFinalResponse && item.Payload["turn_id"] == "late-turn" {
			found = true
		}
	}
	if !found {
		t.Fatal("the mid-expiry final response is not in the republished bundle")
	}
}

// Hooks and retention run in different processes with no ordering between
// them. Whatever the interleaving, a request SaveRequest reported as written
// is never lost to expiry, and a request refused because the session was
// already forgotten leaves no orphan behind.
func TestConcurrentHookRequestIsNeverLostToExpiry(t *testing.T) {
	const rounds = 30
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	expiry := t0.Add(91 * 24 * time.Hour)
	kept, forgotten := 0, 0
	for round := 0; round < rounds; round++ {
		dir := t.TempDir()
		local := newTestStore(t)
		memory := storage.NewMemoryStore()
		id := fmt.Sprintf("s%02d", round)
		if err := local.SaveRegistration(registration(id, writeTranscript(t, dir, id+".jsonl", codexTranscript))); err != nil {
			t.Fatal(err)
		}
		collect(t, local, memory, t0)

		var wg sync.WaitGroup
		var saveErr, sweepErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, sweepErr = Sweep(context.Background(), local, memory, Options{Now: func() time.Time { return expiry }, SessionMaxAge: retentionWindow})
		}()
		evidence := finalResponse(t, expiry)
		go func() {
			defer wg.Done()
			// Spread the hook across the sweep so rounds land before the
			// snapshot, between it and the forget, and after the forget.
			time.Sleep(time.Duration(round) * 40 * time.Microsecond)
			saveErr = local.SaveRequest(id, "stop", expiry, evidence)
		}()
		wg.Wait()
		if sweepErr != nil {
			t.Fatal(sweepErr)
		}
		requests, err := local.LoadRequests()
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case saveErr == nil:
			if registered(t, local) != 1 || len(requests) != 1 {
				t.Fatalf("round %d: a request reported as written was lost (registered=%d requests=%d)", round, registered(t, local), len(requests))
			}
			kept++
		case errors.Is(saveErr, collector.ErrSessionNotRegistered):
			if registered(t, local) != 0 || len(requests) != 0 {
				t.Fatalf("round %d: refused request left state behind (registered=%d requests=%d)", round, registered(t, local), len(requests))
			}
			forgotten++
		default:
			t.Fatalf("round %d: %v", round, saveErr)
		}
	}
	t.Logf("%d rounds: request kept the session %d times, arrived after it was forgotten %d times", rounds, kept, forgotten)
}

// A registration that never received a transcript path can never be
// captured, so a request queued for it is not work the collector will do.
// Once the session is older than the retention window, that request no
// longer defers expiry: the session is forgotten locally, with zero bucket
// calls, and the request's hook text goes with it. The same request on a
// registration with a transcript path still defers expiry.
func TestQueuedRequestDoesNotKeepATranscriptlessSessionPastRetention(t *testing.T) {
	dir := t.TempDir()
	local := newTestStore(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	withPath := registration("with-path", writeTranscript(t, dir, "with-path.jsonl", codexTranscript))
	withoutPath := registration("no-path", "")
	for _, reg := range []archive.SessionRegistration{withPath, withoutPath} {
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
		if err := local.SaveRequest(reg.ArchiveSessionID, "stop", t0.Add(time.Minute), finalResponse(t, t0.Add(time.Minute))); err != nil {
			t.Fatal(err)
		}
	}
	store := &recordingStore{ObjectStore: storage.NewMemoryStore()}

	// Inside the window nothing expires, request or not.
	if result := sweep(t, local, store, t0.Add(retentionWindow-time.Hour), Options{}); len(result.PrunedSessions) != 0 || len(result.Errors) != 0 {
		t.Fatalf("expired inside the retention window: %#v", result)
	}

	result := sweep(t, local, store, t0.Add(retentionWindow+time.Hour), Options{})
	if len(result.Errors) != 0 || len(result.PrunedSessions) != 1 || result.PrunedSessions[0] != "no-path" || len(result.DeletedSessions) != 0 {
		t.Fatalf("result=%#v, want only no-path pruned", result)
	}
	if store.count() != 0 {
		t.Fatalf("a never-captured session cost %d bucket calls", store.count())
	}
	if _, found, _ := local.LoadRegistration("no-path"); found {
		t.Fatal("the transcript-less registration survived past retention")
	}
	if _, found, _ := local.LoadRegistration("with-path"); !found {
		t.Fatal("the request stopped deferring expiry for a session the collector can still capture")
	}
	requests, err := local.LoadRequests()
	if err != nil || len(requests) != 1 || requests[0].ArchiveSessionID != "with-path" {
		t.Fatalf("requests=%#v err=%v; the forgotten session's request must go with it", requests, err)
	}
}

// A session read from Cursor's database has no transcript path by design,
// but the collector still captures it: its queued request keeps deferring
// expiry past the retention window, as a file session's does.
func TestQueuedRequestKeepsACursorDatabaseSessionPastRetention(t *testing.T) {
	local := newTestStore(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reg := registration("cursor-db", "")
	reg.Harness.Name = "cursor"
	reg.SourceKind, reg.SourceKey = archive.SourceKindCursorSQLite, reg.NativeSessionID
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", t0.Add(time.Minute), finalResponse(t, t0.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{ObjectStore: storage.NewMemoryStore()}
	result := sweep(t, local, store, t0.Add(retentionWindow+time.Hour), Options{})
	if len(result.Errors) != 0 || len(result.PrunedSessions) != 0 || len(result.DeletedSessions) != 0 {
		t.Fatalf("result=%#v, want nothing expired", result)
	}
	if _, found, _ := local.LoadRegistration(reg.ArchiveSessionID); !found {
		t.Fatal("unpublished work on a Cursor database session was deleted")
	}
}
