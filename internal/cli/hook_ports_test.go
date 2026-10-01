package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/filechange"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/scheduler"
)

type syntheticHooks struct{ inspections *int }

var syntheticSettings = []byte(`{"peculiar":{"knock":"archive"}}`)

func (syntheticHooks) EnvironmentKeys() []string { return []string{"SYNTHETIC_ROOT"} }
func (syntheticHooks) Location(r agentapi.HookLocations) string {
	return filepath.Join(r.UserHome, "native-weird", "signals.cfg")
}
func (h syntheticHooks) Plan(r agentapi.HookPlanRequest) ([]filechange.Change, error) {
	if r.File.ReadError != nil {
		return nil, r.File.ReadError
	}
	if r.Action == agentapi.HookRemove {
		if !r.File.Present || !bytes.Equal(r.File.Bytes, syntheticSettings) {
			return nil, nil
		}
		return []filechange.Change{{Path: r.File.Path, Before: append([]byte(nil), r.File.Bytes...), Existed: true, Delete: r.File.Regular, Mode: r.File.Mode}}, nil
	}
	return []filechange.Change{{Path: r.File.Path, Before: append([]byte(nil), r.File.Bytes...), After: append([]byte(nil), syntheticSettings...), Existed: r.File.Present, Mode: 0600}}, nil
}
func (h syntheticHooks) Inspect(r agentapi.HookInspectionRequest) (agentapi.HookInspection, error) {
	if h.inspections != nil {
		*h.inspections++
	}
	if r.File.ReadError != nil {
		return agentapi.HookInspection{State: agentapi.HookUnreadable, Reason: "settings_unreadable"}, r.File.ReadError
	}
	if !r.File.Present {
		return agentapi.HookInspection{State: agentapi.HookAbsent, Reason: "settings_absent"}, nil
	}
	owned := bytes.Equal(r.File.Bytes, syntheticSettings)
	state := agentapi.HookForeign
	if owned {
		state = agentapi.HookOwned
	}
	return agentapi.HookInspection{State: state, Installed: owned, Reason: "synthetic_settings"}, nil
}
func registryWithSyntheticHooks(t *testing.T, port agentapi.HookConfigurator) *builtin.Registry {
	t.Helper()
	descriptors := append(agentmeta.Builtins().All(), agentmeta.Descriptor{ID: syntheticID, DisplayName: "Synthetic"})
	catalog, err := agentmeta.New(descriptors)
	if err != nil {
		t.Fatal(err)
	}
	var bindings []builtin.Integration
	for _, d := range agentmeta.Builtins().All() {
		b, _ := productionAgents.Lookup(string(d.ID))
		b.Descriptor = agentmeta.Descriptor{ID: d.ID}
		bindings = append(bindings, b)
	}
	bindings = append(bindings, builtin.Integration{Descriptor: agentmeta.Descriptor{ID: syntheticID}, Launcher: syntheticLauncher{}, Hooks: port})
	registry, err := builtin.New(catalog, bindings)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}
func TestInjectedHookConfiguratorReachesSetupJournalAndStatus(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	inspections := 0
	port := syntheticHooks{&inspections}
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	env.Agents = registryWithSyntheticHooks(t, port)
	executable, err := env.executable()
	if err != nil {
		t.Fatal(err)
	}
	next := config.Config{MachineID: "machine", Harnesses: []string{"synthetic"}, Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "synthetic-bucket", Region: "region", AWSProfile: "profile"}, Archive: archive.Config{SchemaVersion: 1, Enabled: true, MachineID: "machine"}}
	if err := applySetup(home, userHome, executable, config.Config{}, &next, nil, env); err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(userHome, "native-weird", "signals.cfg")
	if next.HookFiles["synthetic"] != expected {
		t.Fatalf("recorded native location %v", next.HookFiles)
	}
	data, err := os.ReadFile(expected)
	if err != nil || !bytes.Equal(data, syntheticSettings) {
		t.Fatalf("shared journal did not apply native plan: %s %v", data, err)
	}
	before := inspections
	view := statusView{Apps: []appStatus{{Name: "synthetic"}}}
	readInstalledApps(&view, next, home, userHome, env)
	if inspections <= before || view.Apps[0].Hooks != "installed" {
		t.Fatalf("status did not consume inspector: %d %+v", inspections, view.Apps)
	}
	changes, found, err := hooks.PlanRemovalOf(env.installedHookFiles(userHome, next), env.installation(home, userHome).owner(), "synthetic")
	if err != nil || !found {
		t.Fatalf("remove plan %v %v", found, err)
	}
	if err := hooks.Apply([]hooks.Change{changes}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(expected); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("synthetic removal did not roundtrip: %v", err)
	}
}
func TestInjectedHookPlanFailureRollsBackSharedSetup(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	env.Agents = registryWithSyntheticHooks(t, syntheticHooks{})
	fakeSched(env).beforeLoad = func(_ scheduler.Ref) error { return errors.New("after files durable") }
	executable, err := env.executable()
	if err != nil {
		t.Fatal(err)
	}
	next := config.Config{MachineID: "machine", Harnesses: []string{"synthetic"}, Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "synthetic-bucket", Region: "region", AWSProfile: "profile"}, Archive: archive.Config{SchemaVersion: 1, Enabled: true, MachineID: "machine"}}
	if err := applySetup(home, userHome, executable, config.Config{}, &next, nil, env); err == nil {
		t.Fatal("injected durable boundary did not fail")
	}
	if _, err := os.Stat(filepath.Join(userHome, "native-weird", "signals.cfg")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("native settings survived rollback: %v", err)
	}
	if _, found, err := config.Load(home); err != nil || found {
		t.Fatalf("active config survived rollback: %v %v", found, err)
	}
}
