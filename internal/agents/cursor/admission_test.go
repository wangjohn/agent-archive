package cursor

import (
	"context"
	"database/sql"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdmissionPassSettledIdentityAndEveryPathAvoidsBackup(t *testing.T) {
	scratch := t.TempDir()
	cursorstore.SnapshotTempDirForTesting = scratch
	t.Cleanup(func() { cursorstore.SnapshotTempDirForTesting = "" })
	path := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(t.Context(), `CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY,value BLOB); INSERT INTO cursorDiskKV VALUES ('composerData:c','{"_v":18,"composerId":"c","createdAt":1789923600000,"workspaceIdentifier":{"id":"w","uri":"file:///synthetic"},"fullConversationHeadersOnly":[{"bubbleId":"b","type":2}]}'),('bubbleId:c:b','{"_v":3,"bubbleId":"b","type":2,"text":"synthetic visible","createdAt":1789923600000}')`)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	ref := agentapi.SourceRef{Kind: archive.SourceKindCursorSQLite, Key: "c"}
	opened, err := (SourceProvider{}).OpenAdmissionPass(t.Context(), agentapi.SourceEnvironment{Database: path}, ref)
	if err != nil {
		t.Fatal(err)
	}
	pass := opened.(*admissionPass)
	ordinary := pass.SourcePass.(*sourcePass)
	ordinary.signature = func(context.Context, string, string) (cursorstore.Signature, error) {
		t.Fatal("ordinary signature invoked")
		return cursorstore.Signature{}, nil
	}
	if _, err = pass.Signature(t.Context(), ref); err == nil {
		t.Fatal("ordinary signature admitted")
	}
	snap, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{RawBytes: 4096, RecordBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	facts := snap.(agentapi.AdmissionCatalogSnapshot).AdmissionChat()
	if facts.ID != "c" || facts.KeyID != "c" || facts.Folder != "/synthetic" || facts.WorkspaceID != "w" || facts.CreatedAt.IsZero() {
		t.Fatal(facts)
	}
	filtered, err := (Filter{}).Filter(t.Context(), snap.Input(), agentapi.FilterContext{})
	if err != nil || len(filtered.Records) == 0 {
		t.Fatal(filtered, err)
	}
	if err = snap.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = pass.Read(ctx, ref, agentapi.ReadLimits{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	pass.budget = &admissionBudget{rows: 100, bytes: 220}
	if _, err = pass.Read(t.Context(), ref, agentapi.ReadLimits{RawBytes: 4096, RecordBytes: 4096}); err == nil {
		t.Fatal("payload budget ignored")
	}
	if ordinary.Attempts() != 0 || ordinary.Snapshots() != 0 {
		t.Fatal("admission prepared ordinary backup")
	}
	if err = pass.Close(); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(scratch); err != nil || len(entries) != 0 {
		t.Fatal("scratch allocated", entries, err)
	}
}

func TestAdmissionPassLiveWALRemainsUnadmittedWithoutScratch(t *testing.T) {
	scratch := t.TempDir()
	cursorstore.SnapshotTempDirForTesting = scratch
	t.Cleanup(func() { cursorstore.SnapshotTempDirForTesting = "" })
	path := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err = db.ExecContext(t.Context(), `PRAGMA journal_mode=WAL; CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY,value BLOB); INSERT INTO cursorDiskKV VALUES ('composerData:c','{"composerId":"c","createdAt":1,"conversation":[{"type":2,"text":"synthetic"}]}')`); err != nil {
		t.Fatal(err)
	}
	ref := agentapi.SourceRef{Kind: archive.SourceKindCursorSQLite, Key: "c"}
	opened, err := (SourceProvider{}).OpenAdmissionPass(t.Context(), agentapi.SourceEnvironment{Database: path}, ref)
	if err != nil {
		t.Fatal(err)
	}
	p := opened.(*admissionPass)
	ordinary := p.SourcePass.(*sourcePass)
	ordinary.signature = func(context.Context, string, string) (cursorstore.Signature, error) {
		t.Fatal("ordinary signature invoked")
		return cursorstore.Signature{}, nil
	}
	if snap, err := p.Read(t.Context(), ref, agentapi.ReadLimits{}); snap != nil || err == nil || !strings.Contains(err.Error(), "unadmitted") {
		t.Fatal(snap, err)
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	if ordinary.Attempts() != 0 || ordinary.Snapshots() != 0 {
		t.Fatal("live admission used backup")
	}
	if entries, err := os.ReadDir(scratch); err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
}
