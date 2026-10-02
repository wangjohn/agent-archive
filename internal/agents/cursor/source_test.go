package cursor

import (
	"context"
	"database/sql"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"path/filepath"
	"testing"
)

func TestReadReusesOnlySuccessfulLiveAdmissionProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), `CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY,value BLOB); INSERT INTO cursorDiskKV VALUES ('composerData:c','{"lastUpdatedAt":1,"fullConversationHeadersOnly":[]}')`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := (SourceProvider{}).OpenPass(context.Background(), agentapi.SourceEnvironment{Database: path})
	if err != nil {
		t.Fatal(err)
	}
	pass := opened.(*sourcePass)
	defer func() {
		if err := pass.Close(); err != nil {
			t.Error(err)
		}
	}()
	probes := 0
	pass.signature = func(ctx context.Context, path, key string) (cursorstore.Signature, error) {
		probes++
		return cursorstore.ReadSignature(ctx, path, key)
	}
	ref := agentapi.SourceRef{Kind: archive.SourceKindCursorSQLite, Key: "c"}
	if _, err = pass.Signature(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	// Change after live admission: the snapshot observation must be the new state.
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), `UPDATE cursorDiskKV SET value='{"lastUpdatedAt":2,"fullConversationHeadersOnly":[]}'`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	snap, err := pass.Read(context.Background(), ref, agentapi.ReadLimits{RawBytes: 1024, RecordBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if probes != 1 || snap.Observation().Signature != (cursorstore.Signature{LastUpdatedAt: 2}).SourceSignature() {
		t.Fatalf("probes=%d observation=%+v", probes, snap.Observation())
	}
	if err = snap.Close(); err != nil {
		t.Fatal(err)
	}
	snap, err = pass.Read(context.Background(), ref, agentapi.ReadLimits{RawBytes: 1024, RecordBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if probes != 2 {
		t.Fatalf("direct Read skipped admission probe: %d", probes)
	}
	if err = snap.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = pass.Read(ctx, ref, agentapi.ReadLimits{}); !errors.Is(err, context.Canceled) || probes != 2 {
		t.Fatalf("cancellation=%v probes=%d", err, probes)
	}
}
