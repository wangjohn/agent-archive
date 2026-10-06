package agentapi

import (
	"errors"
	"sync"
)

// NativeReadBudget bounds charged native extents, immutable caches and borrowed
// scratch across one pass. It is not a bound on filtered maps or process RSS.
type NativeReadBudget struct {
	mu    sync.Mutex
	limit int64
	used  int64
	peak  int64
}

// NewNativeReadBudget creates a pass-owned shared charge ledger.
func NewNativeReadBudget(limit int64) *NativeReadBudget {
	return &NativeReadBudget{limit: max(limit, 0)}
}

// Reserve charges before allocation or borrowing; refusal changes no state.
func (b *NativeReadBudget) Reserve(bytes int64) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if bytes < 0 || bytes > b.limit-b.used {
		return false
	}
	b.used += bytes
	b.peak = max(b.peak, b.used)
	return true
}

// Release returns an exact owned charge. Imbalanced releases panic because
// quietly resetting the ledger would hide a violated safety invariant.
func (b *NativeReadBudget) Release(bytes int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if bytes < 0 || bytes > b.used {
		panic("unbalanced native read budget release")
	}
	b.used -= bytes
}

// Available returns remaining capacity for a bounded optional cache.
func (b *NativeReadBudget) Available() int64 {
	if b == nil {
		return 1 << 62
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limit - b.used
}

// Charged returns current and highest pass charges for measured evidence.
func (b *NativeReadBudget) Charged() (used, peak int64) {
	if b == nil {
		return 0, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used, b.peak
}

// CodexRolloutResourceBudget shares lookup copy charges with source providers.
type CodexRolloutResourceBudget interface{ NativeReadBudget() *NativeReadBudget }

// ErrReadBudget identifies transient shared resource pressure separately from
// permanent record/format ceilings that also use the Limit failure category.
var ErrReadBudget = errors.New("shared read data budget exhausted")

// ReadBudgetLimit keeps resource refusal pending through codec/error wrappers.
func ReadBudgetLimit(cause error) error { return Wrap(Limit, errors.Join(ErrReadBudget, cause)) }
