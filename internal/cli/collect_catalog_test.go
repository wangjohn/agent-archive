package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/rolloutcatalog"
)

func TestPendingCodexCatalogUsesSmallSharedInventoryBudget(t *testing.T) {
	t.Parallel()
	if pendingCodexRollouts(nil)() != nil || pendingCodexRollouts(&config.DiscoveryConfig{})() != nil {
		t.Fatal("unapproved diagnostic created a catalog")
	}
	home := t.TempDir()
	dir := filepath.Join(home, "sessions")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	const entries = 300
	for i := range entries {
		id := fmt.Sprintf("%08x-1111-4111-8111-111111111111", i+1)
		raw := `{"type":"session_meta","payload":{"id":"` + id + `","cwd":"/synthetic","timestamp":"2026-10-01T12:00:00Z","source":"cli","cli_version":"0.160.0","originator":"codex_cli_rs"}}` + "\n"
		if err := os.WriteFile(filepath.Join(dir, "rollout-"+id+".jsonl"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	lookup := pendingCodexRollouts(&config.DiscoveryConfig{Enabled: true, CodexHomes: []string{home}})
	c := lookup().(*rolloutcatalog.Catalog)
	if c.Counters().Headers != 0 || lookup() != c {
		t.Fatal("diagnostic did not remain lazy and shared")
	}
	set, err := c.Thread(t.Context(), "00000001-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	counts := c.Counters()
	if set.Complete || c.Issues()["entry_budget"] == 0 || counts.Entries > 256 || counts.HeaderBytes > 256<<10 || counts.PrefixBytes != 0 {
		t.Fatalf("diagnostic exceeded small metadata budget: set=%#v counts=%#v issues=%#v", set, counts, c.Issues())
	}
}
