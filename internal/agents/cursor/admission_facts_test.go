package cursor

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestAdmissionFactsBindSupportedCodecAndRelationships(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		relationships string
		state         agentapi.CursorRelationshipState
	}{
		{"absent", "", agentapi.CursorRelationshipsAbsent},
		{"empty", `,"subagentComposerIds":[]`, agentapi.CursorRelationshipsValid},
		{"valid", `,"subagentComposerIds":["b","a"]`, agentapi.CursorRelationshipsValid},
		{"null", `,"subagentComposerIds":null`, agentapi.CursorRelationshipsMalformed},
		{"bad", `,"subagentComposerIds":[2]`, agentapi.CursorRelationshipsMalformed},
		{"duplicate", `,"subagentComposerIds":["a","a"]`, agentapi.CursorRelationshipsMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw := []byte(`{"_v":18,"composerId":"c","createdAt":1,"conversation":[{"type":2,"text":"synthetic"}]` + tc.relationships + `}`)
			decoded, ok := decodeComposerData("composerData:c", raw)
			if !ok || !decoded.counted || decoded.chat.CursorFacts.Relationships != tc.state || !decoded.chat.CursorFacts.VersionPresent || decoded.chat.CursorFacts.Version != 18 {
				t.Fatal(decoded, ok)
			}
			if tc.state == agentapi.CursorRelationshipsValid && len(decoded.chat.CursorFacts.ChildIDsSHA256) != 64 {
				t.Fatal(decoded.chat.CursorFacts)
			}
		})
	}
	a, _ := decodeComposerData("composerData:c", []byte(`{"subagentComposerIds":["a","b"]}`))
	b, _ := decodeComposerData("composerData:c", []byte(`{"subagentComposerIds":["b","a"]}`))
	if a.chat.CursorFacts != b.chat.CursorFacts {
		t.Fatal("digest depends on order")
	}
}

func TestAdmissionSameSnapshotParentRoutingRefusesFormerTopLevel(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "native.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), `CREATE TABLE cursorDiskKV(key TEXT PRIMARY KEY,value BLOB); INSERT INTO cursorDiskKV VALUES ('composerData:c','{"composerId":"c","createdAt":1,"conversation":[{"type":2,"text":"synthetic"}]}'),('composerData:parent','{"composerId":"parent","subagentComposerIds":["c"]}')`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	ref := agentapi.SourceRef{Kind: archive.SourceKindCursorSQLite, Key: "c"}
	pass, err := (SourceProvider{}).OpenAdmissionPass(t.Context(), agentapi.SourceEnvironment{Database: path}, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer pass.Close()
	if snapshot, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{}); snapshot != nil || err == nil {
		t.Fatal("admitted newly routed child", snapshot, err)
	}
}
