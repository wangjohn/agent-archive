package cli

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/state"
	"testing"
)

func TestPendingCodexCatalogUsesExistingLazyOwner(t *testing.T) {
	t.Parallel()
	if pendingCodexRollouts(nil)() != nil {
		t.Fatal("missing owner created inventory")
	}
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := discovery.NewCodexRolloutLookup(t.Context(), store, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.CloseReadOnly() }()
	if _, ok := any(owner).(agentapi.CodexRolloutSliceProvider); ok {
		t.Fatal("ordinary lookup acquired full-slice interface")
	}
	lookup := pendingCodexRollouts(owner)
	first := lookup()
	if first != lookup() || first != owner.MetadataInventory() {
		t.Fatal("pending diagnostic created second owner")
	}
	if _, ok := first.(agentapi.CodexRolloutSliceProvider); !ok {
		t.Fatal("explicit view missing validation slice")
	}
}
