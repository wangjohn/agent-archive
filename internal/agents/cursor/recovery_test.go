package cursor

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
)

type evidenceBudget struct {
	rows  int
	bytes int64
}

func (b *evidenceBudget) RemainingRows() int { return b.rows }

func (b *evidenceBudget) RemainingBytes() int64 { return b.bytes }

func (b *evidenceBudget) Charge(rows int, bytes int64) error {
	if rows < 0 || bytes < 0 || rows > b.rows || bytes > b.bytes {
		return agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
	}
	b.rows -= rows
	b.bytes -= bytes
	return nil
}

func TestRecoverySourcePassHonorsRawLimitWithoutAdmissionProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), `CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY,value BLOB)`); err != nil {
		t.Fatal(err)
	}
	value := `{"fullConversationHeadersOnly":[],"unused":"` + strings.Repeat("x", 1000) + `"}`
	_, err = db.ExecContext(t.Context(), `INSERT INTO cursorDiskKV VALUES ('composerData:c',?)`, value)
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	opened, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{Database: path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := opened.Close(); err != nil {
			t.Error(err)
		}
	}()
	pass := opened.(*sourcePass)
	pass.signature = func(context.Context, string, string) (cursorstore.Signature, error) {
		t.Fatal("recovery opened admission signature")
		return cursorstore.Signature{}, nil
	}
	budget := &evidenceBudget{rows: 100, bytes: 4096}
	snapshot, err := opened.(agentapi.RecoverySourcePass).ReadRecovery(t.Context(), agentapi.SourceRef{Kind: archive.SourceKindCursorSQLite, Key: "c"}, agentapi.ReadLimits{RawBytes: 512, RecordBytes: 4096}, budget)
	if snapshot != nil || !agentapi.HasFailure(err, agentapi.Limit) || len(pass.live) != 0 || budget.bytes != 4096-216 {
		t.Fatal("native recovery ignored per-source limit", snapshot, budget, err)
	}
}
