package codex

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
)

const labelTestID = "01900000-0000-7000-8000-000000000001"

func labelFixture(t *testing.T) (string, agentapi.LabelRequest) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "sessions")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout.jsonl")
	reg := archive.SessionRegistration{ArchiveSessionID: "synthetic-label", NativeSessionID: labelTestID, Harness: archive.Harness{Name: "codex", Version: "0.159.2"}, TranscriptPath: path}
	bundle := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: reg.ArchiveSessionID, NativeSessionID: labelTestID, ProjectID: "project", Capture: archive.SourceCapture{Harness: reg.Harness, AdapterName: "codex", AdapterVersion: "0.16.0", CapturedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}, NativeRecords: []map[string]any{{"type": "session_meta", "payload": map[string]any{"id": labelTestID, "cli_version": "0.159.2"}}, {"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "Invented prompt"}}}}
	bundle.Capture.FilterVersion = archive.FilterVersion
	return root, agentapi.LabelRequest{Registration: reg, Bundle: bundle}
}

func labelIndex(t *testing.T, root string, names ...string) {
	t.Helper()
	out := []byte{}
	for i, name := range names {
		b, _ := json.Marshal(map[string]any{"id": labelTestID, "thread_name": name, "updated_at": time.Date(2030-i, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)})
		out = append(out, b...)
		out = append(out, '\n')
	}
	if err := os.WriteFile(filepath.Join(root, "session_index.jsonl"), out, 0600); err != nil {
		t.Fatal(err)
	}
}

func labelDB(t *testing.T, root string, request agentapi.LabelRequest, mode, name, title string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE threads(id TEXT PRIMARY KEY,history_mode TEXT,name TEXT,title TEXT,first_user_message TEXT,preview TEXT,source TEXT,cli_version TEXT,rollout_path TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO threads VALUES(?,?,?,?,?,?,?,?,?)", labelTestID, mode, name, title, "Invented prompt", "Invented prompt", "\"cli\"", "0.159.2", request.Registration.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func lookupLabel(root string, request agentapi.LabelRequest) (archive.SessionLabel, bool) {
	m := (LabelProvider{}).LookupLabels(context.Background(), agentapi.LabelEnvironment{Homes: []string{root}}, []agentapi.LabelRequest{request})
	l, ok := m[request.Registration.ArchiveSessionID]
	return l, ok
}

func TestLabelsIndexUsesLastUsablePhysicalRecord(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	labelIndex(t, root, "Old name", "Latest physical name", "")
	got, ok := lookupLabel(root, request)
	if !ok || got.Name != "Latest physical name" {
		t.Fatalf("%+v %v", got, ok)
	}
	labelIndex(t, root, "Invented prompt")
	got, ok = lookupLabel(root, request)
	if !ok || got.State != "confirmed_absent" {
		t.Fatalf("%+v %v", got, ok)
	}
}

func TestLabelsIndexRequiresVerifiedLegacyContext(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	labelIndex(t, root, "Index name")
	request.Bundle.Capture.FilterVersion = "16"
	if _, ok := lookupLabel(root, request); ok {
		t.Fatal("older filtered source supplied no native history-mode proof")
	}
	request.Bundle.Capture.FilterVersion = archive.FilterVersion
	request.Bundle.NativeRecords[0]["payload"].(map[string]any)["history_mode"] = "paginated"
	if _, ok := lookupLabel(root, request); ok {
		t.Fatal("missing paginated DB was interpreted as legacy index")
	}
}

func TestLabelsSettledCanonicalStoragePrecedence(t *testing.T) {
	for _, tc := range []struct{ mode, name, title, want string }{{"legacy", "Unused name", "Database title", "Database title"}, {"legacy", "Unused name", "Invented prompt", "Index name"}, {"paginated", "Canonical name", "Stale title", "Canonical name"}, {"paginated", "Invented prompt", "Stale title", "Invented prompt"}, {"paginated", "", "Stale title", ""}} {
		t.Run(tc.mode+tc.want, func(t *testing.T) {
			t.Parallel()
			root, request := labelFixture(t)
			labelIndex(t, root, "Index name")
			db := labelDB(t, root, request, tc.mode, tc.name, tc.title)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			got, ok := lookupLabel(root, request)
			if !ok || got.Name != tc.want {
				t.Fatalf("%+v %v want %q", got, ok, tc.want)
			}
		})
	}
}

func TestLabelsUnfinishedIndexAndLiveWALAreUnavailable(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	labelIndex(t, root, "Name")
	f, err := os.OpenFile(filepath.Join(root, "session_index.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("{\"id\":")
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := lookupLabel(root, request); ok {
		t.Fatal("unfinished append was authoritative")
	}
	labelIndex(t, root, "Name")
	db := labelDB(t, root, request, "paginated", "Live name", "")
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE threads SET name='Changed live name'"); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookupLabel(root, request); ok {
		t.Fatal("live WAL was authoritative")
	}
}

func TestLabelsRejectUnknownProducerRelocationAndHome(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	labelIndex(t, root, "Name")
	request.Bundle.Capture.Harness.Version = "0.160.0"
	if _, ok := lookupLabel(root, request); ok {
		t.Fatal("unknown producer accepted")
	}
	request.Bundle.Capture.Harness.Version = "0.159.2"
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("sqlite_home = '/elsewhere'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookupLabel(root, request); ok {
		t.Fatal("relocation accepted")
	}
	if err := os.Remove(filepath.Join(root, "config.toml")); err != nil {
		t.Fatal(err)
	}
	request.Registration.TranscriptPath = "/outside/session.jsonl"
	if _, ok := lookupLabel(root, request); ok {
		t.Fatal("foreign home accepted")
	}
}
