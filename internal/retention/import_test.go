package retention

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
)

var (
	importedAt   = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	longAgoStart = time.Date(2024, 9, 1, 0, 0, 0, 0, time.UTC)
)

func importedRegistration(id, transcriptPath string) archive.SessionRegistration {
	reg := registration(id, transcriptPath)
	reg.SessionStartedAt, reg.RegisteredAt, reg.AdmittedAt = longAgoStart, importedAt, importedAt
	reg.Origin, reg.StartedAtSource, reg.ImportBatch = archive.SessionOriginImport, archive.StartedAtSourceTranscript, "2026-09-23-1"
	return reg
}

// An import's start can be years old. Before it first publishes, retention
// must age it from its admission: aging it from its start would expire it
// the moment its first upload failed or its transcript was not yet read.
func TestImportWithOldStartIsNotExpiredBeforeItPublishes(t *testing.T) {
	dir := t.TempDir()
	local := newTestStore(t)
	// A first upload that fails leaves a pending publication.
	flaky := &flakyStore{ObjectStore: storage.NewMemoryStore(), down: true}
	if err := local.SaveRegistration(importedRegistration("upload", writeTranscript(t, dir, "upload.jsonl", codexTranscript))); err != nil {
		t.Fatal(err)
	}
	if result := collect(t, local, flaky, importedAt); len(result.Errors) != 1 {
		t.Fatalf("expected the first upload to fail: %#v", result)
	}
	// Never scanned: no cache and nothing pending.
	if err := local.SaveRegistration(importedRegistration("unread", filepath.Join(dir, "missing.jsonl"))); err != nil {
		t.Fatal(err)
	}
	// No transcript path yet, with a queued request.
	if err := local.SaveRegistration(importedRegistration("pathless", "")); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRequest("pathless", "backfill", importedAt); err != nil {
		t.Fatal(err)
	}

	store := &recordingStore{ObjectStore: storage.NewMemoryStore()}
	if result := sweep(t, local, store, importedAt.Add(24*time.Hour), Options{}); len(result.PrunedSessions)+len(result.DeletedSessions) != 0 || len(result.Errors) != 0 {
		t.Fatalf("an import was expired by its start: %#v", result)
	}
	if registered(t, local) != 3 {
		t.Fatal("an import was expired by its start")
	}
	flaky.setDown(false)
	if result := collect(t, local, flaky, importedAt.Add(25*time.Hour)); len(result.Published) != 1 {
		t.Fatalf("the retried upload did not publish: %#v", result)
	}

	// A window after admission (and after the capture), all three expire.
	result := sweep(t, local, flaky, importedAt.Add(retentionWindow+time.Hour), Options{})
	if len(result.Errors) != 0 || len(result.DeletedSessions) != 1 || result.DeletedSessions[0] != "upload" || len(result.PrunedSessions) != 2 {
		t.Fatalf("result=%#v", result)
	}
	if registered(t, local) != 0 {
		t.Fatal("imports outlived the retention window")
	}
}

// After a bucket change, an import admitted since belongs to the current
// bucket even though it started long before the change, so retention
// deletes its objects there. A session admitted before the change is only
// pruned locally.
func TestImportAfterDestinationChangeIsDeletedFromCurrentBucket(t *testing.T) {
	dir := t.TempDir()
	local := newTestStore(t)
	cfg := config.Config{DestinationSince: importedAt.Add(-24 * time.Hour)}

	previous := storage.NewMemoryStore()
	earlier := registration("earlier", writeTranscript(t, dir, "earlier.jsonl", codexTranscript))
	earlier.SessionStartedAt = cfg.DestinationSince.Add(-time.Hour)
	earlier.RegisteredAt = earlier.SessionStartedAt
	if err := local.SaveRegistration(earlier); err != nil {
		t.Fatal(err)
	}
	collect(t, local, previous, earlier.SessionStartedAt.Add(time.Minute))

	current := storage.NewMemoryStore()
	if err := local.SaveRegistration(importedRegistration("imported", writeTranscript(t, dir, "imported.jsonl", codexTranscript))); err != nil {
		t.Fatal(err)
	}
	collect(t, local, current, importedAt.Add(time.Minute))
	if meta := fetchMetadata(t, current, "imported"); meta.SessionID != "imported" {
		t.Fatal("import was not published to the current bucket")
	}

	result := sweep(t, local, current, importedAt.Add(retentionWindow+time.Hour), Options{CurrentDestination: cfg.InCurrentDestination})
	if len(result.Errors) != 0 || len(result.DeletedSessions) != 1 || result.DeletedSessions[0] != "imported" || len(result.PrunedSessions) != 1 || result.PrunedSessions[0] != "earlier" {
		t.Fatalf("result=%#v", result)
	}
	metaKey, _ := archive.MetadataObjectKey("codex", "imported")
	if _, err := current.Get(context.Background(), metaKey); err == nil {
		t.Fatal("the import's objects were left in the current bucket")
	}
	if meta := fetchMetadata(t, previous, "earlier"); meta.SessionID != "earlier" {
		t.Fatal("the previous bucket was modified")
	}
}

// Every retention expiry leaves a removal record, so backfill will not
// import the session again: after deleting it from the bucket, after pruning
// a never-published session, and after pruning one from a previous
// destination. A session a hook kept alive gets none.
func TestRetentionExpiryLeavesRemovalRecords(t *testing.T) {
	dir := t.TempDir()
	local := newTestStore(t)
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	store := storage.NewMemoryStore()
	for _, id := range []string{"deleted", "kept"} {
		if err := local.SaveRegistration(registration(id, writeTranscript(t, dir, id+".jsonl", codexTranscript))); err != nil {
			t.Fatal(err)
		}
	}
	collect(t, local, store, t0)
	if err := local.SaveRegistration(registration("unpublished", filepath.Join(dir, "missing.jsonl"))); err != nil {
		t.Fatal(err)
	}
	previous := registration("previous", writeTranscript(t, dir, "previous.jsonl", codexTranscript))
	if err := local.SaveRegistration(previous); err != nil {
		t.Fatal(err)
	}
	past := t0.Add(retentionWindow + time.Hour)
	// A hook resumed "kept" just now; its request defers expiry.
	if err := local.SaveRequest("kept", "sessionstart", past); err != nil {
		t.Fatal(err)
	}
	result := sweep(t, local, store, past, Options{CurrentDestination: func(reg archive.SessionRegistration) bool { return reg.ArchiveSessionID != "previous" }})
	if len(result.Errors) != 0 || len(result.DeletedSessions) != 1 || len(result.PrunedSessions) != 2 {
		t.Fatalf("result=%#v", result)
	}
	for _, id := range []string{"deleted", "unpublished", "previous"} {
		record, found, err := local.Removal("codex", "native-"+id)
		if err != nil || !found || record.Reason != collector.RemovalReasonRetention || !record.At.Equal(past) || record.Harness != "codex" {
			t.Fatalf("%s: record=%#v found=%v err=%v", id, record, found, err)
		}
	}
	if _, found, err := local.Removal("codex", "native-kept"); err != nil || found {
		t.Fatalf("a session retention kept has a removal record: found=%v err=%v", found, err)
	}
}

// A removal record that cannot be written keeps the session registered and
// reports the failure; the next sweep records and forgets it.
func TestRetentionRetriesExpiryWhenTheRemovalRecordFails(t *testing.T) {
	home := t.TempDir()
	local, err := collector.NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRegistration(registration("s1", filepath.Join(home, "missing.jsonl"))); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(home, "forgotten")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemoryStore()
	past := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC).Add(retentionWindow + time.Hour)
	if result := sweep(t, local, store, past, Options{}); len(result.Errors) != 1 || len(result.PrunedSessions) != 0 {
		t.Fatalf("result=%#v", result)
	}
	if registered(t, local) != 1 {
		t.Fatal("the session was forgotten without its removal record")
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if result := sweep(t, local, store, past, Options{}); len(result.Errors) != 0 || len(result.PrunedSessions) != 1 {
		t.Fatalf("retry result=%#v", result)
	}
	if _, found, err := local.Removal("codex", "native-s1"); err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
}
