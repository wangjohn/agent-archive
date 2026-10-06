package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
	"github.com/wangjohn/agent-archive/internal/local"
)

var errCatalogBudget = agentapi.ReadBudgetLimit(errors.New("native catalog exceeds shared data budget"))

// reserveCatalog keeps restored facts and new observation/coverage allocations
// charged for the lookup lifetime. The catalog and its coverage borrow the same
// owner; assigning their pointers does not charge a second decoded document.
func (l *CodexRolloutLookup) reserveCatalog(n int64) bool {
	if !l.readBudget.Reserve(n) {
		return false
	}
	l.catalogCharge += n
	return true
}

func (l *CodexRolloutLookup) readCatalog(ctx context.Context, path string, c *catalog) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	n := info.Size()
	if !info.Mode().IsRegular() || n < 0 {
		return errors.New("native catalog is not a regular file")
	}
	if !l.readBudget.Reserve(n) {
		return errCatalogBudget
	}
	defer l.readBudget.Release(n)
	if !l.readBudget.Reserve(n) {
		return errCatalogBudget
	}
	keep := false
	defer func() {
		if !keep {
			l.readBudget.Release(n)
		}
	}()
	raw := make([]byte, n)
	if _, err := io.ReadFull(f, raw); err != nil {
		return err
	}
	var extra [1]byte
	if count, err := f.Read(extra[:]); count != 0 || !errors.Is(err, io.EOF) {
		return errCatalogBudget
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, c); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l.catalogCharge += n
	keep = true
	return nil
}

func (l *CodexRolloutLookup) writeCatalog(ctx context.Context, path string, c catalog) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	const countScratch = 32 << 10
	if !l.readBudget.Reserve(countScratch) {
		return errCatalogBudget
	}
	n, err := jsonwire.Bound(ctx, c, l.readBudget.Available()-1)
	l.readBudget.Release(countScratch)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return errors.Join(errCatalogBudget, err)
	}
	if !l.readBudget.Reserve(n + 1) {
		return errCatalogBudget
	}
	defer l.readBudget.Release(n + 1)
	if err := ctx.Err(); err != nil {
		return err
	}
	return local.WriteCompact(path, c)
}

func (l *CodexRolloutLookup) retainObservation(ctx context.Context, entry cached) bool {
	const countScratch = 32 << 10
	if !l.readBudget.Reserve(countScratch) {
		return false
	}
	n, err := jsonwire.Bound(ctx, entry, l.readBudget.Available())
	l.readBudget.Release(countScratch)
	return err == nil && l.reserveCatalog(n)
}
