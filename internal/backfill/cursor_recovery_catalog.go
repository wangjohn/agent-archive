package backfill

import (
	"context"
	"database/sql"
	"errors"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
)

func readCursorRecoveryDatabase(ctx context.Context, catalogs agentapi.DatabaseCatalogLookup, path string, maxRows int, maxBytes int64) CursorDatabaseResult {
	if catalogs == nil || maxRows <= 0 || maxBytes <= 0 {
		return unchecked(cursorstore.UnknownFormat)
	}
	inspector, ok := catalogs.LookupDatabaseCatalog("cursor")
	if !ok {
		return unchecked(cursorstore.UnknownFormat)
	}
	var res CursorDatabaseResult
	budget := &cursorRecoveryReadBudget{remaining: maxBytes, rows: cursorRecoveryRecords}
	err := cursorstore.ReadRecovery(ctx, path, budget, func(ctx context.Context, db *sql.DB) error {
		host := &recoveryCatalogHost{db: db, maxRows: maxRows, budget: budget}
		catalog, err := inspector.InspectCatalog(ctx, host)
		res = CursorDatabaseResult{Chats: catalog.Chats, Checked: err == nil, RecoveryRows: cursorRecoveryRecords - budget.rows, RecoveryBytes: maxBytes - budget.remaining, recoveryReadBudget: budget, RecoveryBudgetExhausted: agentapi.HasFailure(err, agentapi.Limit)}
		return err
	})
	if errors.Is(err, cursorstore.ErrNoDatabase) {
		return CursorDatabaseResult{Checked: true}
	}
	res.RecoveryRows = cursorRecoveryRecords - budget.rows
	res.RecoveryBytes = maxBytes - budget.remaining
	res.recoveryReadBudget = budget
	res.RecoveryBudgetExhausted = res.RecoveryBudgetExhausted || agentapi.HasFailure(err, agentapi.Limit)
	if err != nil {
		res.Checked = false
		res.Reason = cursorstore.ReasonOf(err)
	}
	return res
}

// recoveryCatalogHost applies bounds to the native owner's indexed query,
// without knowing native selectors or decoding any native value.
type recoveryCatalogHost struct {
	db      *sql.DB
	maxRows int
	budget  *cursorRecoveryReadBudget
	payload int64
}

func (h *recoveryCatalogHost) Query(ctx context.Context, query string, visit func(agentapi.DatabaseRecord) error) (err error) {
	tx, err := h.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, tx.Rollback()) }()
	// Both length preflight and allocated values share this read transaction.
	// The native query retains its schema/index capability checks and range.
	if err := h.preflight(ctx, tx, query); err != nil {
		return err
	}
	// #nosec G202 -- native catalog owner supplies static SQL; limits are bound parameters.
	rows, err := tx.QueryContext(ctx, "SELECT key, value FROM ("+query+") LIMIT ?", h.maxRows)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	count, bytes := 0, int64(0)
	for rows.Next() {
		var key string
		var value []byte
		if err := rows.Scan(&key, &value); err != nil {
			return err
		}
		size := int64(len(key)) + int64(len(value))
		count++
		bytes += size
		if count > h.maxRows || bytes > h.payload {
			return agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
		}
		if err := visit(agentapi.DatabaseRecord{Key: key, Value: value}); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (h *recoveryCatalogHost) preflight(ctx context.Context, tx *sql.Tx, query string) (err error) {
	// #nosec G202 -- native catalog owner supplies static SQL; limits are bound parameters.
	rows, err := tx.QueryContext(ctx, "SELECT length(CAST(key AS BLOB)), length(CAST(value AS BLOB)) FROM ("+query+") LIMIT ?", h.maxRows+1)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	count, remaining := 0, h.budget.remaining
	var payload int64
	for rows.Next() {
		if err := h.budget.Charge(1, 16); err != nil {
			return err
		}
		var key, value sql.NullInt64
		if err := rows.Scan(&key, &value); err != nil {
			return err
		}
		count++
		if count > h.maxRows || key.Int64 > remaining || value.Int64 > remaining-key.Int64 {
			return agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
		}
		remaining -= key.Int64 + value.Int64
		payload += key.Int64 + value.Int64
	}
	if err := rows.Err(); err != nil {
		return err
	}
	h.payload = payload
	return h.budget.Charge(count, payload)
}
