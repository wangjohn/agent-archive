package discovery

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/local"

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
	room := maxCatalogBytes - lookup.catalogCharge - lookup.restoredCharge
	if lookup.reserveCatalog(room+1) || !lookup.reserveCatalog(room) || lookup.reserveCatalog(1) {
		t.Fatal("catalog charge not capped", lookup.catalogCharge)
	}
}

// A restored catalog over the cap is rebuilt, as a corrupt one is, and one
// a reset discards gives its charge back: either way discovery goes on and
// writes a new catalog, rather than failing every pass.
func TestOversizedCatalogIsRebuilt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		size  int
		reset bool
	}{{"over the cap", maxCatalogBytes + 1<<20, false}, {"discarded by a reset", 20 << 20, true}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			store, cfg, at, root := fixture(t)
			writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 1, "sessions")
			path := filepath.Join(store.Home(), "discovery-catalog.json")
			old := catalog{Version: catalogVersion, Roots: []string{"/elsewhere"}, Health: Health{Errors: []string{strings.Repeat("x", c.size)}}}
			if err := local.WriteCompact(path, old); err != nil {
				t.Fatal(err)
			}
			lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
			if err != nil {
				t.Fatal("lookup refused an oversized catalog:", err)
			}
			if lookup.restoredCharge != 0 {
				t.Fatalf("discarded catalog still charged: %d", lookup.restoredCharge)
			}
			h, err := run(t.Context(), store, cfg, Options{Sources: builtin.NewBuiltins(), Rollouts: lookup, Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport)
			if err != nil || h.Probes == 0 {
				t.Fatalf("discovery made no progress: probes=%d errors=%v err=%v", h.Probes, h.Errors, err)
			}
			if !c.reset && !slices.Contains(h.Errors, "catalog_rebuilt") {
				t.Fatalf("over-cap catalog not reported rebuilt: %v", h.Errors)
			}
			if err := lookup.Close(); err != nil {
				t.Fatal(err)
			}
			if info, err := os.Stat(path); err != nil || info.Size() >= 1<<20 {
				t.Fatalf("catalog not rewritten: %v %v", info, err)
			}
		})
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
