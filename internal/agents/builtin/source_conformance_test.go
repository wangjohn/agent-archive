package builtin

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/testutil/agenttest"
)

func TestFileSourceConsistencyConformance(t *testing.T) {
	registry := NewBuiltins()
	for _, name := range []string{"claude", "codex", "cursor"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "transcript.jsonl")
			if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			provider, _, _ := registry.LookupSources(name)
			agenttest.Sources(t, provider, agentapi.SourceEnvironment{}, agentapi.SourceRef{Path: path}, agentapi.SourceRef{Path: path + "-missing"})
		})
	}
}

func TestDatabaseSourceConsistencyConformance(t *testing.T) {
	for _, model := range []string{"closed-immutable", "running-backup"} {
		t.Run(model, func(t *testing.T) { databaseSourceConformance(t, model == "running-backup") })
	}
}

func databaseSourceConformance(t *testing.T, running bool) {
	t.Helper()
	cursorstore.SnapshotTempDirForTesting = t.TempDir()
	t.Cleanup(func() { cursorstore.SnapshotTempDirForTesting = "" })
	path := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if running {
		if _, err := db.ExecContext(t.Context(), `PRAGMA journal_mode=WAL`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY,value BLOB); INSERT INTO cursorDiskKV VALUES ('composerData:c','{"lastUpdatedAt":1,"fullConversationHeadersOnly":[{"bubbleId":"missing"}]}')`); err != nil {
		t.Fatal(err)
	}
	if !running {
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	provider, _, _ := NewBuiltins().LookupSources("cursor")
	agenttest.Sources(t, provider, agentapi.SourceEnvironment{Database: path}, agentapi.SourceRef{Kind: archive.SourceKindCursorSQLite, Key: "c"}, agentapi.SourceRef{Kind: archive.SourceKindCursorSQLite, Key: "absent"})
}
