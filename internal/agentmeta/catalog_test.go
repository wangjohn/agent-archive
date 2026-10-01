package agentmeta

import (
	"reflect"
	"strings"
	"testing"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
	"github.com/wangjohn/agent-archive/internal/testutil/importgraph"
)

const (
	idA         ID        = "a"
	idB         ID        = "b"
	idTest      ID        = "test"
	idMutated   ID        = "mutated"
	idUnsafe    ID        = "../unsafe"
	idUpper     ID        = "Upper"
	opImaginary Operation = "imaginary"
	opMutated   Operation = "mutated"
)

func TestCatalogValidationAndCopies(t *testing.T) {
	for _, ds := range [][]Descriptor{
		{{ID: idUnsafe, DisplayName: "Unsafe"}}, {{ID: idUpper, DisplayName: "Upper"}}, {{ID: idA, DisplayName: ""}},
		{{ID: idA, DisplayName: "A"}, {ID: idA, DisplayName: "A"}},
		{{ID: idA, DisplayName: "A", Aliases: []string{"b"}}, {ID: idB, DisplayName: "B"}},
		{{ID: idA, DisplayName: "A", Aliases: []string{"a"}}},
		{{ID: idA, DisplayName: "A", Aliases: []string{" spaced "}}},
		{{ID: idA, DisplayName: "A", Operations: []Operation{Launch, Launch}}},
		{{ID: idA, DisplayName: "A", Operations: []Operation{opImaginary}}},
	} {
		if _, err := New(ds); err == nil {
			t.Fatalf("accepted invalid descriptors %+v", ds)
		}
	}
	ds := []Descriptor{{ID: idTest, Aliases: []string{"test-alias"}, DisplayName: "Test", Operations: []Operation{Launch}}}
	c, err := New(ds)
	if err != nil {
		t.Fatal(err)
	}
	ds[0].Aliases[0] = "mutated"
	ds[0].Operations[0] = opMutated
	d, ok := c.Lookup(" TEST-ALIAS ")
	if !ok || d.ID != idTest {
		t.Fatalf("lookup: %+v %v", d, ok)
	}
	d.Aliases[0] = "mutated"
	d.Operations[0] = opMutated
	all := c.All()
	all[0].Aliases[0] = "mutated"
	all[0].Operations[0] = opMutated
	all[0].ID = idMutated
	d, _ = c.Lookup("test")
	if d.Aliases[0] != "test-alias" || d.Operations[0] != Launch {
		t.Fatalf("mutable catalog %+v", d)
	}
	if got := Canonical(c, " Future-Agent "); got != "future-agent" {
		t.Fatal(got)
	}
}

func TestPresentationOrders(t *testing.T) {
	if got := Names(Builtins()); !reflect.DeepEqual(got, []string{"claude", "codex", "cursor"}) {
		t.Fatal(got)
	}
	if got := SetupNames(Builtins()); !reflect.DeepEqual(got, []string{"codex", "claude", "cursor"}) {
		t.Fatal(got)
	}
}

func TestIdentityAndIntegrationImportBoundaries(t *testing.T) {
	prefix := "github.com/wangjohn/agent-archive/internal/"
	direct, _ := importgraph.Imports(t, prefix+"agentmeta")
	for _, name := range direct {
		if strings.Contains(name, ".") {
			t.Errorf("agentmeta imports non-standard package %s", name)
		}
	}
	for _, pkg := range []string{"archive", "config", "reader", "agentapi", "agents/claude", "agents/codex", "agents/cursor"} {
		_, all := importgraph.Imports(t, prefix+pkg)
		importgraph.Forbid(t, pkg, all, prefix+"agents/builtin", prefix+"cli", prefix+"capture", prefix+"collector", prefix+"backfill")
	}
}

func TestCanonicalDoesNotCopyDescriptors(t *testing.T) {
	for _, name := range []string{"claude", "claude-code", "future-agent"} {
		if allocs := testing.AllocsPerRun(100, func() { Canonical(Builtins(), name) }); allocs != 0 {
			t.Fatalf("canonical %s allocates %g", name, allocs)
		}
	}
}
