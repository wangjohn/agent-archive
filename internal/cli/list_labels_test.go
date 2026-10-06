package cli

import (
	"context"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
)

type forbiddenListLabels struct{ t *testing.T }

func (p forbiddenListLabels) LookupLabels(context.Context, agentapi.LabelEnvironment, []agentapi.LabelRequest) map[string]archive.SessionLabel {
	p.t.Fatal("listing queried native label storage")
	return nil
}

func TestListUsesArchivedNamesWithoutNativeLookupOrSourceRead(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.add(t, "named", "Invented prompt", archive.ProjectID(a.dir), func(m *archive.Metadata) { m.Name = "Archived native name" })
	base := builtin.NewBuiltins()
	integrations := []builtin.Integration{}
	for _, descriptor := range base.Catalog().All() {
		integration, _ := base.Lookup(string(descriptor.ID))
		integration.Descriptor = agentmeta.Descriptor{ID: descriptor.ID}
		integration.Labels = forbiddenListLabels{t}
		integrations = append(integrations, integration)
	}
	registry, err := builtin.New(base.Catalog(), integrations)
	if err != nil {
		t.Fatal(err)
	}
	a.env.Agents = registry
	a.env.LookPath = func(string) (string, error) { t.Fatal("listing discovered a native executable"); return "", nil }
	store := &scopeBudgetStore{MemoryStore: a.mem}
	a.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
	_, stderr, code := a.runList(t, "--json")
	if code != 0 {
		t.Fatalf("list failed %d: %s", code, stderr)
	}
	if store.sourceGets.Load() != 0 {
		t.Fatal("listing read source rather than archived metadata")
	}
}
