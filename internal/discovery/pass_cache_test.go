package discovery

import (
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
)

// The metadata inventory lives for the pass: no session's scope counts what
// it holds, and releasing it in a later session does not lower that
// session's own count.
func TestMetadataInventoryIsNoSessionsCharge(t *testing.T) {
	t.Parallel()
	store, _, _, root := fixture(t)
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lookup.Close() }()
	budget := lookup.readBudget
	view := lookup.MetadataInventory().(*metadataInventory)
	end := budget.BeginScope(1 << 20)
	if !view.reserve(4<<20) || budget.Available() != 1<<20 {
		t.Fatal("the inventory counted toward the session that built it", budget.Available())
	}
	end()
	end = budget.BeginScope(1 << 20)
	defer end()
	if !budget.Reserve(512 << 10) {
		t.Fatal("reserve")
	}
	if err := view.close(); err != nil {
		t.Fatal(err)
	}
	if budget.Reserve(600<<10) || !budget.ScopeExceeded() {
		t.Fatal("releasing the inventory lowered a later session's count")
	}
	budget.Release(512 << 10)
}

// What the lookup keeps for its whole life stops at maxCatalogBytes,
// however much the pass ledger has free.
func TestCatalogChargeIsCapped(t *testing.T) {
	t.Parallel()
	store, _, _, root := fixture(t)
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lookup.Close() }()
	room := maxCatalogBytes - lookup.catalogCharge
	if lookup.reserveCatalog(room+1) || !lookup.reserveCatalog(room) || lookup.reserveCatalog(1) {
		t.Fatal("catalog charge not capped", lookup.catalogCharge)
	}
}

// The caps on what the pass keeps between sessions fit the room the pass
// ledger keeps for them beside a session's ceiling.
func TestPassCacheCapsFitTheReserve(t *testing.T) {
	t.Parallel()
	if total := PassCacheBytes + codex.PassCacheBytes; total > agentapi.PassCacheReserve {
		t.Fatalf("pass caches %d MiB exceed the %d MiB reserve", total>>20, agentapi.PassCacheReserve>>20)
	}
}
