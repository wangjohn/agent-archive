package builtin

import (
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

const (
	testID                 agentmeta.ID        = "test"
	aliasID                agentmeta.ID        = "alias"
	unknownID              agentmeta.ID        = "unknown"
	brokenOperation        agentmeta.Operation = "broken"
	unimplementedOperation agentmeta.Operation = "unimplemented"
)

type fakeLauncher struct{}

func (*fakeLauncher) Executables() agentapi.Executables {
	return agentapi.Executables{Names: []string{"test"}, Install: "Test"}
}

func (*fakeLauncher) Args(r agentapi.LaunchRequest) ([]string, error) { return []string{r.Prompt}, nil }

func TestRegistryConstructorMatrix(t *testing.T) {
	c, err := agentmeta.New([]agentmeta.Descriptor{{ID: testID, Aliases: []string{"alias"}, DisplayName: "Test"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(nil, nil); err == nil {
		t.Fatal("accepted nil catalog")
	}
	valid := Integration{Descriptor: agentmeta.Descriptor{ID: testID}, Launcher: &fakeLauncher{}}
	var typedNil *fakeLauncher
	for _, tc := range []struct {
		name     string
		bindings []Integration
		wantErr  bool
	}{
		{"launch-only", []Integration{valid}, false},
		{"missing", nil, true}, {"duplicate", []Integration{valid, valid}, true},
		{"nil", []Integration{{Descriptor: valid.Descriptor}}, true},
		{"typed-nil", []Integration{{Descriptor: valid.Descriptor, Launcher: typedNil}}, true},
		{"unknown", []Integration{{Descriptor: agentmeta.Descriptor{ID: unknownID}, Launcher: &fakeLauncher{}}}, true},
		{"alias-binding", []Integration{{Descriptor: agentmeta.Descriptor{ID: aliasID}, Launcher: &fakeLauncher{}}}, true},
		{"metadata-override", []Integration{{Descriptor: agentmeta.Descriptor{ID: testID, DisplayName: "Override"}, Launcher: &fakeLauncher{}}}, true},
		{"operation-promise", []Integration{{Descriptor: agentmeta.Descriptor{ID: testID, Operations: []agentmeta.Operation{agentmeta.Launch}}, Launcher: &fakeLauncher{}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(c, tc.bindings)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
	r, err := New(c, []Integration{valid})
	if err != nil {
		t.Fatal(err)
	}
	b, ok := r.Lookup(" ALIAS ")
	if !ok || b.Descriptor.Operations[0] != agentmeta.Launch {
		t.Fatalf("lookup %+v %v", b, ok)
	}
	b.Descriptor.Aliases[0] = "broken"
	b.Descriptor.Operations[0] = brokenOperation
	supporting := r.Supporting(agentmeta.Launch)
	supporting[0].Descriptor.Aliases[0] = "broken"
	supporting[0].Descriptor.Operations[0] = brokenOperation
	d, _ := r.Catalog().Lookup("alias")
	if d.Aliases[0] != "alias" || d.Operations[0] != agentmeta.Launch {
		t.Fatalf("mutable registry %+v", d)
	}
	if len(r.Supporting(unimplementedOperation)) != 0 {
		t.Fatal("fabricated operation")
	}
}

type fakeRuntime struct{}

func (*fakeRuntime) Detect(agentapi.RuntimeEnvironment) agentapi.RuntimeObservation {
	return agentapi.RuntimeObservation{NativeID: "native", PresenceKey: "TEST_RUNTIME"}
}

func (*fakeRuntime) SessionEnvironmentKeys() []string { return []string{"TEST_RUNTIME", "TEST_PARENT"} }

func TestRuntimeProjectionAndCleanupUnion(t *testing.T) {
	t.Parallel()
	c, err := agentmeta.New([]agentmeta.Descriptor{{ID: testID, DisplayName: "Test"}, {ID: aliasID, DisplayName: "Alias"}})
	if err != nil {
		t.Fatal(err)
	}
	bindings := []Integration{{Descriptor: agentmeta.Descriptor{ID: testID}, Runtime: &fakeRuntime{}}, {Descriptor: agentmeta.Descriptor{ID: aliasID}, Launcher: &fakeLauncher{}, Runtime: &fakeRuntime{}}}
	r, err := New(c, bindings)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Launcher(string(testID)); ok {
		t.Fatal("runtime-only agent fabricated launcher")
	}
	if len(r.Supporting(agentmeta.Runtime)) != 2 || len(r.Supporting(agentmeta.Launch)) != 1 {
		t.Fatal("wrong operation projection")
	}
	d, _ := r.Catalog().Lookup(string(testID))
	if len(d.Operations) != 1 || d.Operations[0] != agentmeta.Runtime {
		t.Fatalf("runtime-only projection %+v", d)
	}
	observations := r.Observations(agentapi.RuntimeEnvironment{})
	if len(observations) != 2 || observations[0].Agent != testID || observations[1].Agent != aliasID {
		t.Fatalf("runtime order %+v", observations)
	}
	keys := r.SessionEnvironmentKeys()
	if len(keys) != 2 || keys[0] != "TEST_RUNTIME" || keys[1] != "TEST_PARENT" {
		t.Fatalf("cleanup union %q", keys)
	}
	keys[0] = "MUTATED"
	if r.SessionEnvironmentKeys()[0] != "TEST_RUNTIME" {
		t.Fatal("mutable cleanup union")
	}
	var typedNil *fakeRuntime
	bindings[0].Runtime = typedNil
	if _, err := New(c, bindings); err == nil {
		t.Fatal("accepted typed-nil runtime")
	}
}
