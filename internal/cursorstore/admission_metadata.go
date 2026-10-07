package cursorstore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// ReadAdmissionComposer decodes selected input and bounded metadata inspection
// on one immutable transaction. Inspection borrows native rows; it cannot
// allocate uncharged payloads or change the snapshot used by the filter. An
// optional reserved workspace binds live-image readback to a rooted held file.
func ReadAdmissionComposer(ctx context.Context, path, id string, rawLimit, recordLimit int64, budget agentapi.RecoveryReadBudget, inspect func(context.Context, agentapi.DatabaseCatalogHost) error, workspace ...agentapi.TemporaryWorkspaceBudget) (c Composer, err error) {
	read := func(ctx context.Context, db *sql.DB) (err error) {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, tx.Rollback()) }()
		c, err = readRecoveryComposerTx(ctx, tx, id, rawLimit, recordLimit, budget)
		if err != nil {
			return err
		}
		if inspect != nil {
			return inspect(ctx, admissionMetadataHost{tx: tx, budget: budget})
		}
		return nil
	}
	if len(workspace) == 0 {
		err = ReadRecovery(ctx, path, budget, read)
	} else {
		err = readAdmissionWorkspace(ctx, workspace[0], budget, read)
	}
	return c, err
}

func readAdmissionWorkspace(ctx context.Context, workspace agentapi.TemporaryWorkspaceBudget, budget agentapi.RecoveryReadBudget, read func(context.Context, *sql.DB) error) (err error) {
	if err = ctx.Err(); err != nil {
		return err
	}
	rooted, ok := workspace.(interface{ OpenWorkspace() (*os.Root, error) })
	if !ok || budget == nil {
		return NotChecked(Locked)
	}
	root, err := rooted.OpenWorkspace()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	f, err := root.Open("native.db")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() > admissionImageLimit {
		return NotChecked(Unreadable)
	}
	path := filepath.Join(workspace.Root(), "native.db")
	vfs, err := newAdmissionDestinationVFS(ctx, path, f, 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, vfs.Close()) }()
	ctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()
	db, err := sql.Open("sqlite", admissionDSN(dsn(path, false), vfs.name))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	db.SetMaxOpenConns(1)
	if err = budget.Charge(0, 200); err != nil {
		return err
	}
	conn, err := admissionConnection(ctx, db)
	if err != nil {
		return err
	}
	if err = conn.Close(); err != nil {
		return err
	}
	if err = validateRecoverySchema(ctx, db, budget); err != nil {
		return err
	}
	err = read(ctx, db)
	after, statErr := f.Stat()
	if statErr != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return errors.Join(err, NotChecked(ChangedDuringRead))
	}
	return errors.Join(err, ctx.Err())
}

type admissionMetadataHost struct {
	tx     *sql.Tx
	budget agentapi.RecoveryReadBudget
}

func (h admissionMetadataHost) Query(ctx context.Context, query string, visit func(agentapi.DatabaseRecord) error) (err error) {
	if err = ctx.Err(); err != nil {
		return err
	}
	if query != `SELECT key, value FROM cursorDiskKV WHERE key >= 'composerData:' AND key < 'composerData;'` {
		return NotChecked(UnknownFormat)
	}

	remaining := h.budget.RemainingRows()
	if remaining <= 0 {
		return agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
	}
	n, err := h.metadataLengthCount(ctx, remaining)
	if err != nil {
		return err
	}
	rows, err := h.tx.QueryContext(ctx, `SELECT key,value FROM cursorDiskKV WHERE key >= 'composerData:' AND key < 'composerData;' LIMIT ?`, n+1)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	seen := 0
	for rows.Next() {
		if seen >= n {
			return agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
		}
		var record agentapi.DatabaseRecord
		if err = rows.Scan(&record.Key, &record.Value); err != nil {
			return err
		}
		if err = visit(record); err != nil {
			return err
		}
		seen++
	}
	if seen != n {
		return NotChecked(ChangedDuringRead)
	}
	return rows.Err()
}

func (h admissionMetadataHost) metadataLengthCount(ctx context.Context, remaining int) (n int, err error) {
	lengths, err := h.tx.QueryContext(ctx, `SELECT length(CAST(key AS BLOB)),length(CAST(value AS BLOB)) FROM cursorDiskKV WHERE key >= 'composerData:' AND key < 'composerData;' LIMIT ?`, remaining+1)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, lengths.Close()) }()
	n = 0
	for lengths.Next() {
		var keyBytes int64
		var valueBytes sql.NullInt64
		if err = lengths.Scan(&keyBytes, &valueBytes); err != nil {
			break
		}
		if keyBytes < 0 || valueBytes.Int64 < 0 || keyBytes > h.budget.RemainingBytes()-valueBytes.Int64 {
			err = agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
			break
		}
		if err = h.budget.Charge(2, keyBytes+valueBytes.Int64+16); err != nil {
			break
		}
		n++
	}
	return n, errors.Join(err, lengths.Err())

}
