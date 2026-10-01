package config

import (
	"github.com/wangjohn/agent-archive/internal/agentmeta"
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
