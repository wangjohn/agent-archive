package cursorstore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
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

func (b *recoveryTestBudget) RemainingRows() int { return b.rows }

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
	c, err := ReadRecoveryComposer(t.Context(), path, "c", 0, 1<<20, b)
	if !agentapi.HasFailure(err, agentapi.Limit) || len(c.Composer) != 0 || len(c.Bubbles) != 0 {
		t.Fatal(c, err)
	}
	if _, ok := agentapi.ErrorObservation(err); ok {
		t.Fatal("failure allocated admission signature")
	}
	if b.usedRows != 2 || b.usedBytes != 216 {
		t.Fatalf("unexpected pre-allocation ledger: %+v", b)
	}
}

func TestRecoveryComposerRefusesLiveWALWithoutBackup(t *testing.T) {
	root := useTempSnapshots(t)
	path := StateDatabase(t.TempDir())
	startWriter(t, path).put(chatRows())
	b := &recoveryTestBudget{rows: 100, bytes: 1 << 20}
	c, err := ReadRecoveryComposer(t.Context(), path, "c", 0, 1<<20, b)
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
	c, err := ReadRecoveryComposer(t.Context(), path, "c", 0, 1<<20, b)
	if err != nil || len(c.Bubbles) != 3 || c.Bubbles[1].Missing || c.Bubbles[1].Value != nil {
		t.Fatal(c, err)
	}
	if b.usedRows == 0 || b.usedBytes <= 200 {
		t.Fatal("missing evidence accounting", b)
	}
	limited := &recoveryTestBudget{rows: 2, bytes: 1 << 20}
	c, err = ReadRecoveryComposer(t.Context(), path, "c", 0, 1<<20, limited)
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

func TestRecoveryComposerHonorsPerSourceRawAllowance(t *testing.T) {
	path := StateDatabase(t.TempDir())
	rows := toAny(chatRows())
	writeDB(t, path, false, rows)
	b := &recoveryTestBudget{rows: 100, bytes: 1 << 20}
	// The epoch has ample space, but this source's allowance cannot hold its
	// header and messages. Length preflight must refuse before bubble allocation.
	c, err := ReadRecoveryComposer(t.Context(), path, "c", int64(len(rows["composerData:c"].(string)))+1, 1<<20, b)
	if !agentapi.HasFailure(err, agentapi.Limit) || len(c.Composer) != 0 || len(c.Bubbles) != 0 {
		t.Fatal("per-source allowance was ignored", c, err)
	}
}

func TestRecoveryCancelledBeforeNativeObservation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	b := &recoveryTestBudget{rows: 100, bytes: 1 << 20}
	err := ReadRecovery(ctx, StateDatabase(t.TempDir()), b, func(context.Context, *sql.DB) error {
		t.Fatal("query after cancellation")
		return nil
	})
	if !errors.Is(err, context.Canceled) || b.usedBytes != 0 || b.usedRows != 0 {
		t.Fatal("cancelled recovery observed a source", b, err)
	}
}

func TestRecoveryMetadataExhaustionPrecedesSQL(t *testing.T) {
	for _, budget := range []*recoveryTestBudget{{rows: 0, bytes: 4096}, {rows: 100, bytes: 15}} {
		// No native table exists: attempting even the first metadata query would
		// return a schema failure. Exhausted allowance must stop before that query.
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "empty.vscdb"))
		if err != nil {
			t.Fatal(err)
		}
		tx, err := db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		_, readErr := recoveryBubbleLengths(t.Context(), tx, "c", budget)
		cleanup := errors.Join(tx.Rollback(), db.Close())
		if !agentapi.HasFailure(readErr, agentapi.Limit) || cleanup != nil || budget.usedRows != 0 || budget.usedBytes != 0 {
			t.Fatal("metadata query preceded budget gate", budget, readErr, cleanup)
		}
	}
}

func TestRecoveryRefusesNonUniqueNativeKeysBeforePayloadReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), `CREATE TABLE cursorDiskKV (key TEXT,value BLOB)`); err != nil {
		t.Fatal(err)
	}
	for _, row := range [][2]string{{"composerData:c", chat("c", 1, "b")}, {"bubbleId:c:b", strings.Repeat("x", 2048)}, {"bubbleId:c:b", bubble("small")}} {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO cursorDiskKV VALUES (?,?)`, row[0], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	budget := &recoveryTestBudget{rows: 100, bytes: 8192}
	c, err := ReadRecoveryComposer(t.Context(), path, "c", 4096, 512, budget)
	if err == nil || ReasonOf(err) != UnknownFormat || len(c.Composer) != 0 || len(c.Bubbles) != 0 {
		t.Fatal("duplicate keys bypassed native length/allocation proof", c, err)
	}
}

func TestRecoveryAcceptsNativeUniqueKeyIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), `CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE,value BLOB)`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	budget := &recoveryTestBudget{rows: 10, bytes: 1024}
	called := false
	err = ReadRecovery(t.Context(), path, budget, func(context.Context, *sql.DB) error { called = true; return nil })
	if err != nil || !called || budget.usedRows != 1 || budget.usedBytes != 208 {
		t.Fatal(err, called, budget)
	}
}

func TestRecoveryRefusesExplicitNativeCollation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), `CREATE TABLE cursorDiskKV (key TEXT COLLATE NOCASE,value BLOB); CREATE UNIQUE INDEX native_key ON cursorDiskKV(key COLLATE BINARY)`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	budget := &recoveryTestBudget{rows: 10, bytes: 1024}
	err = ReadRecovery(t.Context(), path, budget, func(context.Context, *sql.DB) error {
		t.Fatal("unknown collation authorized payload queries")
		return nil
	})
	if err == nil || ReasonOf(err) != UnknownFormat {
		t.Fatal(err)
	}
}
