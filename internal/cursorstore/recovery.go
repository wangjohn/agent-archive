package cursorstore

import (
	"context"
	"database/sql"
	"errors"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// ReadRecovery reads settled in-place evidence without preparing a snapshot.
// Header observations and all query payloads share the caller's epoch budget.
// Live WAL sources require a fresh settled observation and are never copied.
func ReadRecovery(ctx context.Context, path string, budget agentapi.RecoveryReadBudget, read func(context.Context, *sql.DB) error) error {
	if budget == nil {
		return agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
	}
	// resolve and immutable post-read verification each read a 100-byte header.
	if err := budget.Charge(0, 200); err != nil {
		return err
	}
	src, err := resolve(path)
	if err != nil {
		return err
	}
	if src.live {
		return NotChecked(Locked)
	}
	return readInPlace(ctx, src, Options{}, read)
}

// ReadRecoveryComposer returns bounded immutable evidence with the existing
// native relationship decoder, without live signatures or failure signatures.
func ReadRecoveryComposer(ctx context.Context, path, id string, recordLimit int64, budget agentapi.RecoveryReadBudget) (c Composer, err error) {
	if id == "" {
		return c, ErrComposerNotFound
	}
	err = ReadRecovery(ctx, path, budget, func(ctx context.Context, db *sql.DB) (err error) {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, tx.Rollback()) }()
		value, err := recoveryComposerValue(ctx, tx, id, recordLimit, budget)
		if err != nil {
			return err
		}
		_, ids, err := decodeHeaders(value)
		if err != nil {
			return err
		}
		lengths, err := recoveryBubbleLengths(ctx, tx, id, budget)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		prefix := bubblePrefix(id)
		for _, key := range ids {
			if seen[key] {
				continue
			}
			seen[key] = true
			n, present := lengths[key]
			if !present {
				continue
			}
			if recordLimit > 0 && n > recordLimit {
				return agentapi.Wrap(agentapi.Limit, ErrRecordLimit)
			}
			if err := budget.Charge(1, int64(len(prefix+key))+n); err != nil {
				return err
			}
		}
		found, err := listedBubbles(ctx, tx, id, ids, lengths)
		if err != nil {
			return err
		}
		c = Composer{Composer: value, Bubbles: make([]Bubble, len(ids))}
		for i, key := range ids {
			_, present := lengths[key]
			c.Bubbles[i] = Bubble{ID: key, Value: found[key], Missing: !present}
		}
		return nil
	})
	return c, err
}

func recoveryComposerValue(ctx context.Context, tx *sql.Tx, id string, recordLimit int64, budget agentapi.RecoveryReadBudget) ([]byte, error) {
	if err := budget.Charge(1, 8); err != nil {
		return nil, err
	}
	var size sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT length(CAST(value AS BLOB)) FROM cursorDiskKV WHERE key = ?`, "composerData:"+id).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !size.Valid {
		return nil, ErrComposerNotFound
	}
	if err != nil {
		return nil, err
	}
	if recordLimit > 0 && size.Int64 > recordLimit {
		return nil, agentapi.Wrap(agentapi.Limit, ErrRecordLimit)
	}
	if err := budget.Charge(1, size.Int64); err != nil {
		return nil, err
	}
	return composerRow(ctx, tx, id)
}

func recoveryBubbleLengths(ctx context.Context, tx *sql.Tx, id string, budget agentapi.RecoveryReadBudget) (out map[string]int64, err error) {
	prefix := bubblePrefix(id)
	// Charge numeric length projections before reading or allocating native keys.
	rows, err := tx.QueryContext(ctx, `SELECT length(CAST(key AS BLOB)), length(CAST(value AS BLOB)) FROM cursorDiskKV WHERE key >= ? AND key < ? LIMIT ?`, prefix, bubbleUpper(prefix), budget.RemainingRows()+1)
	if err != nil {
		return nil, err
	}
	remainingRows := budget.RemainingRows()
	count, total := 0, int64(0)
	for rows.Next() {
		// The extra row proves incomplete membership; it cannot authorize a root.
		if err := budget.Charge(1, 16); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		var key, size sql.NullInt64
		if err := rows.Scan(&key, &size); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		count++
		total += key.Int64 + 8
		if count > remainingRows || total > budget.RemainingBytes() {
			return nil, errors.Join(agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit), rows.Close())
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if err := budget.Charge(count, total); err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT key,length(CAST(value AS BLOB)) FROM cursorDiskKV WHERE key >= ? AND key < ? LIMIT ?`, prefix, bubbleUpper(prefix), count)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	out = map[string]int64{}
	for rows.Next() {
		var key string
		var size sql.NullInt64
		if err := rows.Scan(&key, &size); err != nil {
			return nil, err
		}
		out[key[len(prefix):]] = size.Int64
	}
	return out, rows.Err()
}
