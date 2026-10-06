package codex

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
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
	bundle := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: reg.ArchiveSessionID, NativeSessionID: labelTestID, ProjectID: "project", Capture: archive.SourceCapture{Harness: reg.Harness, AdapterName: "codex", AdapterVersion: "0.16.0", FilterVersion: archive.FilterVersion, CapturedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}, NativeRecords: []map[string]any{{"type": "session_meta", "payload": map[string]any{"id": labelTestID, "cli_version": "0.159.2"}}, {"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "Invented prompt"}}}}
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

func labelDB(t *testing.T, root string, request agentapi.LabelRequest, mode codexmeta.HistoryMode, name, title string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), "CREATE TABLE threads(id TEXT PRIMARY KEY,history_mode TEXT,name TEXT,title TEXT,first_user_message TEXT,preview TEXT,source TEXT,cli_version TEXT,rollout_path TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), "INSERT INTO threads VALUES(?,?,?,?,?,?,?,?,?)", labelTestID, mode, name, title, "Invented prompt", "Invented prompt", "\"cli\"", "0.159.2", request.Registration.TranscriptPath); err != nil {
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
	if !ok || got.State != archive.SessionLabelAbsent {
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

func TestLabelsIndexMatchesNativeRequiredStringAndUUIDSemantics(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	entries := []map[string]any{
		{"id": labelTestID, "thread_name": "Old name", "updated_at": "unknown"},
		{"id": strings.ToUpper(labelTestID), "thread_name": "Current name", "updated_at": ""},
		{"id": labelTestID, "thread_name": "Missing required stamp"},
	}
	data := []byte{}
	for _, entry := range entries {
		encoded, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, encoded...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(filepath.Join(root, "session_index.jsonl"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if label, ok := lookupLabel(root, request); !ok || label.Name != "Current name" {
		t.Fatalf("%+v %v", label, ok)
	}
}

func TestLabelsSettledReadDoesNotMutateNativeHome(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	labelIndex(t, root, "Index name")
	db := labelDB(t, root, request, codexmeta.CodexHistoryPaginated, "Database name", "Preview title")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	type fileState struct {
		Size     int64
		Modified time.Time
		SHA      [32]byte
	}
	snapshot := func() map[string]fileState {
		t.Helper()
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]fileState{}
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				t.Fatal(err)
			}
			var sum [32]byte
			if info.Mode().IsRegular() {
				data, err := os.ReadFile(filepath.Join(root, entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				sum = sha256.Sum256(data)
			}
			out[entry.Name()] = fileState{Size: info.Size(), Modified: info.ModTime(), SHA: sum}
		}
		return out
	}
	before := snapshot()
	if label, ok := lookupLabel(root, request); !ok || label.Name != "Database name" {
		t.Fatalf("%+v %v", label, ok)
	}
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("read-only resolver changed native contents, metadata or directory entries")
	}
}

func TestLabelsSettledCanonicalStoragePrecedence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode  codexmeta.HistoryMode
		name  string
		title string
		want  string
	}{{codexmeta.CodexHistoryLegacy, "Unused name", "Database title", "Database title"}, {codexmeta.CodexHistoryLegacy, "Unused name", "Invented prompt", "Index name"}, {codexmeta.CodexHistoryPaginated, "Canonical name", "Stale title", "Canonical name"}, {codexmeta.CodexHistoryPaginated, "Invented prompt", "Stale title", "Invented prompt"}, {codexmeta.CodexHistoryPaginated, "", "Stale title", ""}} {
		t.Run(string(tc.mode)+tc.want, func(t *testing.T) {
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
	db := labelDB(t, root, request, codexmeta.CodexHistoryPaginated, "Live name", "")
	if _, err := db.ExecContext(context.Background(), "PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), "UPDATE threads SET name='Changed live name'"); err != nil {
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
