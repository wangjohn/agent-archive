package cursor

import (
	"context"
	"database/sql"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/testutil/agenttest"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadReusesOnlySuccessfulLiveAdmissionProbe(t *testing.T) {
	cursorstore.SnapshotTempDirForTesting = t.TempDir()
	t.Cleanup(func() { cursorstore.SnapshotTempDirForTesting = "" })
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

func TestCursorRecordSourceConformance(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(t.Context(), `CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY,value BLOB); INSERT INTO cursorDiskKV VALUES ('composerData:c','{"lastUpdatedAt":1,"fullConversationHeadersOnly":[{"bubbleId":"b","type":2}]}'),('bubbleId:c:b','{"type":2,"text":"synthetic visible"}')`)
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("database fixture: %v %v", err, closeErr)
	}
	agenttest.RecordSource(t, SourceProvider{}, agentapi.SourceRef{Kind: archive.SourceKindCursorSQLite, Key: "c"}, agentapi.SourceEnvironment{Database: path})
}

func TestTextFallbackRetainsReadFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.txt")
	if err := os.WriteFile(path, []byte("user: hello\nassistant: hi\n"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := transcriptio.Open(transcriptio.OS{}, path, transcriptio.OpenPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.Close() }()
	fault := errors.New("synthetic text read fault")
	input := &textFailedReadInput{FileInput: snapshot, fault: fault}
	_, err = (Filter{}).Filter(t.Context(), agentapi.NativeInput{File: input}, agentapi.FilterContext{StartedAt: time.Unix(1, 0)})
	if input.starts != 2 || !errors.Is(err, fault) || !agentapi.HasFailure(err, agentapi.Unavailable) || agentapi.Deterministic(err) {
		t.Fatalf("text refusal hid retryable read: starts=%d err=%v", input.starts, err)
	}
}

type textFailedReadInput struct {
	agentapi.FileInput
	fault  error
	starts int
}

func (f *textFailedReadInput) ReadAt(p []byte, off int64) (int, error) {
	if off == 0 {
		f.starts++
		if f.starts == 2 {
			return 0, f.fault
		}
	}
	return f.FileInput.ReadAt(p, off)
}
