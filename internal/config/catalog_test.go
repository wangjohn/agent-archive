package config

import (
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"reflect"
	"testing"
)

const testCatalogID agentmeta.ID = "test"

func TestInjectedCatalogNormalizesConfiguration(t *testing.T) {
	c, err := agentmeta.New([]agentmeta.Descriptor{{ID: testCatalogID, Aliases: []string{"test-alias"}, DisplayName: "Test"}})
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	cfg := Config{Handoff: HandoffConfig{Args: map[string][]string{" TEST-ALIAS ": {"--model", "synthetic"}}, DefaultTo: map[string]string{"Test": "TEST-ALIAS"}}}
	if err := SaveWithCatalog(home, cfg, c); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := LoadWithCatalog(home, c)
	if err != nil || !found {
		t.Fatalf("load %v %v", found, err)
	}
	if loaded.Handoff.DefaultTo["test"] != "test" || len(loaded.Handoff.Args["test"]) != 2 {
		t.Fatalf("not canonical %+v", loaded.Handoff)
	}
	if _, _, err := Load(home); err == nil {
		t.Fatal("default catalog accepted unknown agent")
	}
	cfg.Handoff.Args["test"] = []string{"--other"}
	if err := SaveWithCatalog(home, cfg, c); err == nil {
		t.Fatal("accepted conflicting alias keys")
	}
	if _, ok := cfg.Handoff.Args[" TEST-ALIAS "]; !ok {
		t.Fatal("save mutated caller")
	}
}

func TestProtectedConfigurationPreservesCatalogAndMachineSettings(t *testing.T) {
	t.Parallel()
	catalog, err := agentmeta.New([]agentmeta.Descriptor{{ID: testCatalogID, Aliases: []string{"test-alias"}, DisplayName: "Test"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := discoveryConfig(t)
	spares := 3
	cfg.SpareKeys = &spares
	cfg.SpareCredentialRefs = []string{"issued-00000000000000000000000000000001"}
	cfg.MachineName = "synthetic-machine"
	cfg.CloudflareTokenCommand = []string{"synthetic-token-command", "--account"}
	cfg.MCPServerNames = map[string]string{"synthetic-server": "Synthetic"}
	cfg.Handoff = HandoffConfig{Args: map[string][]string{"test": {"--model", "synthetic"}}}
	home := t.TempDir()
	if err := SaveWithCatalog(home, cfg, catalog); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := LoadWithCatalog(home, catalog)
	if err != nil || !found || !reflect.DeepEqual(cfg, loaded) {
		t.Fatalf("protected config lost integrated settings: %#v, %v", loaded, err)
	}
	if _, _, err := Load(home); err == nil {
		t.Fatal("protected configuration bypassed injected catalog validation")
	}
}
