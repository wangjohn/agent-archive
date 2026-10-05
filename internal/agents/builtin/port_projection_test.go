package builtin

import (
	"reflect"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
)

func TestPhase6PortLookupAvoidsMetadataCopies(t *testing.T) {
	registry := NewBuiltins()
	lookups := []struct {
		name   string
		lookup func() bool
	}{
		{"skill alias", func() bool { _, ok := registry.LookupSkills("claude-code"); return ok }},
		{"native header", func() bool { _, ok := registry.LookupNativeHeaders("codex"); return ok }},
		{"history inspector", func() bool { _, ok := registry.LookupImport("claude"); return ok }},
		{"discovery", func() bool { _, ok := registry.LookupDiscovery("claude"); return ok }},
		{"workspace", func() bool { _, ok := registry.LookupWorkspace("cursor"); return ok }},
		{"database", func() bool { _, ok := registry.LookupDatabaseCatalog("cursor"); return ok }},
	}
	for _, lookup := range lookups {
		if !lookup.lookup() {
			t.Fatalf("%s unavailable", lookup.name)
		}
		if allocations := testing.AllocsPerRun(100, func() { lookup.lookup() }); allocations != 0 {
			t.Errorf("%s port lookup copied metadata: %g allocations", lookup.name, allocations)
		}
	}
	before := registry.Catalog().All()
	full, _ := registry.Lookup("claude-code")
	full.Descriptor.Aliases[0] = "changed"
	full.Descriptor.Operations[0] = brokenOperation
	projected := registry.Supporting(agentmeta.Skills)
	projected[0].Descriptor.Operations[0] = brokenOperation
	names := registry.SkillAgents()
	names[0] = "changed"
	if !reflect.DeepEqual(before, registry.Catalog().All()) || registry.SkillAgents()[0] == "changed" {
		t.Fatal("narrow projection exposed mutable metadata")
	}
	if p, ok := registry.LookupSkills(" CLAUDE-CODE "); !ok || p == nil {
		t.Fatal("normalized alias lost")
	}
	if p, ok := registry.LookupWorkspace("claude"); ok || p != nil {
		t.Fatal("absent capability fabricated")
	}
}
