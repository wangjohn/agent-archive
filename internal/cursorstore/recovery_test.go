package cursorstore

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

type recoveryTestBudget struct {
	rows      int
	bytes     int64
	usedRows  int
	usedBytes int64
}

func (b *recoveryTestBudget) RemainingRows() int    { return b.rows }
func (b *recoveryTestBudget) RemainingBytes() int64 { return b.bytes }
func (b *recoveryTestBudget) Charge(rows int, bytes int64) error {
	if rows < 0 || bytes < 0 || rows > b.rows || bytes > b.bytes {
		return agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
	}
	b.rows -= rows
	b.bytes -= bytes
	b.usedRows += rows
	b.usedBytes += bytes
	return nil
}

func TestRecoveryComposerBoundsSignatureRowsAndFailureAllocations(t *testing.T) {
	path := StateDatabase(t.TempDir())
	writeDB(t, path, false, map[string]any{"composerData:c": strings.Repeat("x", 2048)})
	b := &recoveryTestBudget{rows: 100, bytes: 1024}
	c, err := ReadRecoveryComposer(t.Context(), path, "c", 1<<20, b)
	if !agentapi.HasFailure(err, agentapi.Limit) || len(c.Composer) != 0 || len(c.Bubbles) != 0 {
		t.Fatal(c, err)
	}
	if _, ok := agentapi.ErrorObservation(err); ok {
		t.Fatal("failure allocated admission signature")
	}
	if b.usedRows != 1 || b.usedBytes != 208 {
		t.Fatalf("unexpected pre-allocation ledger: %+v", b)
	}
}

func TestRecoveryComposerRefusesLiveWALWithoutBackup(t *testing.T) {
	root := useTempSnapshots(t)
	path := StateDatabase(t.TempDir())
	startWriter(t, path).put(chatRows())
	b := &recoveryTestBudget{rows: 100, bytes: 1 << 20}
	c, err := ReadRecoveryComposer(t.Context(), path, "c", 1<<20, b)
	if ReasonOf(err) != Locked || len(c.Composer) != 0 {
		t.Fatal(c, err)
	}
	entries, err := os.ReadDir(root)
	if (err != nil && !os.IsNotExist(err)) || len(entries) != 0 {
		t.Fatal("recovery attempted a backup", entries, err)
	}
	if b.usedBytes != 200 || b.usedRows != 0 {
		t.Fatal(b)
	}
}

func TestRecoveryComposerPreservesRelationshipsAndBoundsMetadataRows(t *testing.T) {
	path := StateDatabase(t.TempDir())
	rows := toAny(chatRows())
	rows["bubbleId:c:b2"] = nil
	writeDB(t, path, false, rows)
	b := &recoveryTestBudget{rows: 100, bytes: 1 << 20}
	c, err := ReadRecoveryComposer(t.Context(), path, "c", 1<<20, b)
	if err != nil || len(c.Bubbles) != 3 || c.Bubbles[1].Missing || c.Bubbles[1].Value != nil {
		t.Fatal(c, err)
	}
	if b.usedRows == 0 || b.usedBytes <= 200 {
		t.Fatal("missing evidence accounting", b)
	}
	limited := &recoveryTestBudget{rows: 2, bytes: 1 << 20}
	c, err = ReadRecoveryComposer(t.Context(), path, "c", 1<<20, limited)
	if !agentapi.HasFailure(err, agentapi.Limit) || len(c.Composer) != 0 {
		t.Fatal("unbounded metadata rows", c, err)
	}
	r := NewReader(path)
	defer closeOrFail(t, r)
	ordinary, _, err := r.ReadComposerLimited(context.Background(), "c", 1<<20, 1<<20)
	if err != nil || len(ordinary.Bubbles) != 3 {
		t.Fatal("ordinary read changed", ordinary, err)
	}
}
