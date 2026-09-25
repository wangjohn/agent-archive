package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// putCursorRows writes rows into the fixture's Cursor database, as Cursor
// would, replacing any with the same key.
func (f *backfillFixture) putCursorRows(t *testing.T, rows map[string]string) {
	t.Helper()
	db, err := sql.Open("sqlite", backfill.CursorStateDatabase(f.userHome))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	for k, v := range rows {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, k, v); err != nil {
			t.Fatal(err)
		}
	}
}

// publishedCursorSession is the metadata and source bundle the bucket holds
// for reg.
func publishedCursorSession(t *testing.T, bucket storage.ObjectStore, reg archive.SessionRegistration) (archive.Metadata, archive.SourceBundle) {
	t.Helper()
	key, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := bucket.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	source, err := bucket.Get(context.Background(), metadata.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := archive.ReadSourceBundle(bytes.NewReader(source), archive.DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return metadata, bundle
}

func bundleJSON(t *testing.T, bundle archive.SourceBundle) string {
	t.Helper()
	data, err := json.Marshal(bundle.NativeRecords)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A Cursor chat only Cursor's database holds is planned, imported, and
// published end to end, as format cursor-composer with started_at_source
// cursor_composer; a second run is a no-op; a rewrite Cursor makes later is
// published with a cursor_chat_rewritten gap; undo counts the chat, which
// changed after the import, as resumed and deletes it; and a later plan
// skips it as removed_by_undo.
func TestBackfillCursorDatabaseChat(t *testing.T) {
	t.Parallel()
	f, bucket := newImportFixture(t)
	backdateTranscripts(t, f)
	if out, errOut, code := f.importRun(t, nil, false, "--yes"); code != 0 {
		t.Fatalf("import: code %d, %s\n%s", code, errOut, out)
	}
	parents, _ := importRegistrations(t, f.data, firstImport)
	var reg archive.SessionRegistration
	for _, p := range parents {
		if p.NativeSessionID == "k-db-only" {
			reg = p
		}
	}
	if reg.SourceKind != archive.SourceKindCursorSQLite || reg.SourceKey != "k-db-only" || reg.TranscriptPath != "" ||
		reg.DestinationID == "" || reg.Origin != archive.SessionOriginImport || reg.StartedAtSource != archive.StartedAtSourceCursorComposer ||
		!reg.SessionStartedAt.Equal(time.UnixMilli(1789923600000)) {
		t.Fatalf("registration %+v", reg)
	}
	metadata, bundle := publishedCursorSession(t, bucket, reg)
	if metadata.Origin != archive.SessionOriginImport || metadata.StartedAtSource != archive.StartedAtSourceCursorComposer ||
		bundle.Capture.SourceFormat != "cursor-composer" || !strings.Contains(bundleJSON(t, bundle), "Why does the upload retry twice?") {
		t.Fatalf("published %+v / %+v", metadata, bundle.Capture)
	}

	// A second run finds nothing new to import and changes nothing.
	before := bucketSnapshot(t, bucket)
	out, errOut, code := f.importRun(t, nil, false, "--yes")
	if code != 0 || !strings.Contains(out, "Nothing to import.") {
		t.Fatalf("second run: code %d, %s\n%s", code, errOut, out)
	}
	if after := bucketSnapshot(t, bucket); after != before {
		t.Fatal("the second run changed the bucket")
	}

	// Cursor rewrites the finished prompt an hour after the import: the next
	// pass publishes the rewritten chat, with the gap, rather than blocking it.
	later := backfillNow.Add(time.Hour)
	f.env.Now = func() time.Time { return later }
	f.putCursorRows(t, map[string]string{
		"composerData:k-db-only": `{"_v":18,"composerId":"k-db-only","createdAt":1789923600000,"lastUpdatedAt":` + jsonNumber(later.UnixMilli()) + `,"status":"completed",` +
			`"workspaceIdentifier":{"id":"ws1"},"fullConversationHeadersOnly":[{"bubbleId":"m1","type":1},{"bubbleId":"m2","type":2}]}`,
		"bubbleId:k-db-only:m1": `{"_v":3,"bubbleId":"m1","type":1,"text":"Why does the upload retry three times?","createdAt":1789923600000}`,
	})
	if out, errOut, code := f.command(t, "sync"); code != 0 {
		t.Fatalf("sync: code %d, %s\n%s", code, errOut, out)
	}
	if _, blocked, _ := state.OpenReadOnly(f.data).LoadBlocked(reg.ArchiveSessionID); blocked {
		t.Fatal("the rewritten chat is blocked")
	}
	metadata, bundle = publishedCursorSession(t, bucket, reg)
	gap := false
	for _, g := range metadata.CaptureGaps {
		gap = gap || g.Code == collector.CaptureGapCursorChatRewritten
	}
	if !gap || !strings.Contains(bundleJSON(t, bundle), "retry three times") || strings.Contains(bundleJSON(t, bundle), "retry twice") {
		t.Fatalf("rewrite not republished: gaps %+v", metadata.CaptureGaps)
	}

	// Undo: the chat changed after the import, so it counts as resumed.
	out, errOut, code = f.undoRun(t, strings.NewReader("y\n"), true)
	if code != 0 || !strings.Contains(out, "This includes 1 session resumed since the import") {
		t.Fatalf("undo: code %d, %s\n%s", code, errOut, out)
	}
	if keys := sessionKeys(t, bucket, reg); len(keys) != 0 {
		t.Fatalf("left %v", keys)
	}
	record, found, err := state.OpenReadOnly(f.data).Removal("cursor", "k-db-only")
	if err != nil || !found || record.Reason != state.RemovalReasonUndo {
		t.Fatalf("removal record %+v %v %v", record, found, err)
	}
	// Every session of the import, the database chat among them.
	skipped := planJSONFor(t, f)["skipped"].(map[string]any)
	if skipped["removed_by_undo"] != float64(12) {
		t.Fatalf("skipped %v", skipped)
	}
}

func jsonNumber(n int64) string {
	data, _ := json.Marshal(n)
	return string(data)
}
