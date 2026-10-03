package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	"github.com/wangjohn/agent-archive/internal/state"
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

func registryWithSyntheticHooks(t *testing.T, port agentapi.HookConfigurator, decoders ...agentapi.HookDecoder) *builtin.Registry {
	t.Helper()
	descriptors := append(agentmeta.Builtins().All(), agentmeta.Descriptor{ID: syntheticID, DisplayName: "Synthetic", Aliases: []string{"native-synth"}})
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
	var decoder agentapi.HookDecoder
	if len(decoders) > 0 {
		decoder = decoders[0]
	}
	bindings = append(bindings, builtin.Integration{Descriptor: agentmeta.Descriptor{ID: syntheticID}, Launcher: syntheticLauncher{}, Hooks: port, Decoder: decoder})
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
	next := config.Config{MachineID: "machine", Harnesses: []string{"synthetic"}, Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "synthetic-bucket", Region: "region", AWSProfile: "profile"}, Archive: archive.Config{SchemaVersion: 1, Enabled: true, MachineID: "machine", Projects: []archive.ProjectActivation{{Root: t.TempDir(), Included: true}}}}
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
	next := config.Config{MachineID: "machine", Harnesses: []string{"synthetic"}, Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "synthetic-bucket", Region: "region", AWSProfile: "profile"}, Archive: archive.Config{SchemaVersion: 1, Enabled: true, MachineID: "machine", Projects: []archive.ProjectActivation{{Root: t.TempDir(), Included: true}}}}
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

type syntheticHookDecoder struct{}

func (syntheticHookDecoder) Decode(_ context.Context, input agentapi.HookInput) ([]agentapi.LifecycleEvent, error) {
	identity, _ := input.Payload["opaque"].(string)
	project, _ := input.Payload["tree"].(string)
	mode, _ := input.Payload["mode"].(string)
	return []agentapi.LifecycleEvent{{Kind: agentapi.EventStart, Session: agentapi.NativeSession{Agent: syntheticID, NativeID: identity, Mode: agentapi.NativeMode(mode)}, ProjectRoot: project, Reason: "arrived", NativeEvent: "arrived", Start: agentapi.StartEvidence{Kind: agentapi.FreshExplicit, Reason: agentapi.FreshnessExplicitStart}, Evidence: []archive.SupplementalEvidence{{Kind: archive.EvidenceKindLifecycleHook, ObservedAt: input.ObservedAt, Provenance: "hook:synthetic:arrived", Payload: map[string]any{"event_name": "arrived"}}}}}, nil
}

func TestInjectedDecoderReachesActualHookCommandThroughDeclaredAlias(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	env := testEnv(t, home, at)
	env.Agents = registryWithSyntheticHooks(t, syntheticHooks{}, syntheticHookDecoder{})
	env.repoKey = func(string) string { return "" }
	payload := `{"opaque":" exact / λ ","mode":"future native mode","tree":` + quoteJSON(project) + `,"not_retained":"raw native data"}`
	var stderr bytes.Buffer
	for range 2 {
		if code := runHookCommand([]string{"--harness", "native-synth"}, strings.NewReader(payload), &stderr, env); code != 0 || stderr.Len() != 0 {
			t.Fatalf("code %d stderr %s", code, &stderr)
		}
	}
	registrations, err := state.OpenReadOnly(home).LoadRegistrations()
	if err != nil || len(registrations) != 1 {
		t.Fatalf("registrations %+v %v", registrations, err)
	}
	registration := registrations[0]
	if registration.NativeSessionID != " exact / λ " || registration.Harness.Name != string(syntheticID) || registration.Harness.Mode != "future native mode" || !registration.SessionStartedAt.Equal(at) {
		t.Fatalf("lost native or shared facts %+v", registration)
	}
}

type observedHookDecoder struct {
	decode func(agentapi.HookInput) []agentapi.LifecycleEvent
}

func (d observedHookDecoder) Decode(_ context.Context, input agentapi.HookInput) ([]agentapi.LifecycleEvent, error) {
	return d.decode(input), nil
}

func TestHookCommandPreservesDecoderObservationForValidation(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprintf("invalid=%t", invalid), func(t *testing.T) {
			t.Parallel()
			home, project := t.TempDir(), t.TempDir()
			setUpTestConfig(t, home, project, at.Add(-time.Hour))
			env := testEnv(t, home, at)
			observed := time.Time{}
			decoder := observedHookDecoder{decode: func(input agentapi.HookInput) []agentapi.LifecycleEvent {
				observed = input.ObservedAt
				batch, _ := (syntheticHookDecoder{}).Decode(context.Background(), input)
				if invalid {
					batch[0].Evidence[0].ObservedAt = at.Add(time.Hour)
				}
				return batch
			}}
			env.Agents = registryWithSyntheticHooks(t, syntheticHooks{}, decoder)
			env.repoKey = func(string) string { return "" }
			payload := `{"opaque":"native","tree":` + quoteJSON(project) + `}`
			var stderr bytes.Buffer
			if code := runHookCommand([]string{"--harness", "native-synth"}, strings.NewReader(payload), &stderr, env); code != 0 {
				t.Fatalf("hook exit %d", code)
			}
			if !observed.Equal(at) {
				t.Errorf("decoder observed %s, want shared hook time %s", observed, at)
			}
			regs, err := state.OpenReadOnly(home).LoadRegistrations()
			if err != nil {
				t.Fatal(err)
			}
			if invalid {
				if len(regs) != 0 || !strings.Contains(stderr.String(), "inconsistent lifecycle observation time") {
					t.Fatalf("invalid decoder observation admitted: %+v stderr %s", regs, &stderr)
				}
			} else if len(regs) != 1 || stderr.Len() != 0 {
				t.Fatalf("valid decoder observation refused: %+v stderr %s", regs, &stderr)
			}
		})
	}
}

func TestMalformedHookInputDoesNotReachInjectedDecoder(t *testing.T) {
	t.Parallel()
	for _, payload := range []string{"not json", "", `{"opaque":`, "null", "[]"} {
		t.Run(payload, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			env := testEnv(t, home, time.Now())
			called := false
			decoder := observedHookDecoder{decode: func(agentapi.HookInput) []agentapi.LifecycleEvent {
				called = true
				return nil
			}}
			env.Agents = registryWithSyntheticHooks(t, syntheticHooks{}, decoder)
			var stderr bytes.Buffer
			if code := runHookCommand([]string{"--harness", "native-synth"}, strings.NewReader(payload), &stderr, env); code != 0 || stderr.Len() != 0 || called {
				t.Fatalf("malformed hook dispatched: code %d stderr %s called %t", code, &stderr, called)
			}
		})
	}
}

func TestHookClockPanicNeverAdmitsDecodedFacts(t *testing.T) {
	t.Parallel()
	for _, decoderPanics := range []bool{false, true} {
		t.Run(fmt.Sprintf("decoderPanics=%t", decoderPanics), func(t *testing.T) {
			t.Parallel()
			home, project := t.TempDir(), t.TempDir()
			at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			setUpTestConfig(t, home, project, at.Add(-time.Hour))
			env := testEnv(t, home, at)
			clockCalls, decoderCalls, policyCalls := 0, 0, 0
			env.Now = func() time.Time { clockCalls++; panic("clock failure") }
			env.repoKey = func(string) string { policyCalls++; return "" }
			decoder := observedHookDecoder{decode: func(input agentapi.HookInput) []agentapi.LifecycleEvent {
				decoderCalls++
				if decoderPanics {
					panic("decoder failure")
				}
				batch, _ := (syntheticHookDecoder{}).Decode(context.Background(), input)
				return batch
			}}
			env.Agents = registryWithSyntheticHooks(t, syntheticHooks{}, decoder)
			payload := `{"opaque":"native","tree":` + quoteJSON(project) + `}`
			var stderr bytes.Buffer
			if code := runHookCommand([]string{"--harness", "native-synth"}, strings.NewReader(payload), &stderr, env); code != 0 || !strings.Contains(stderr.String(), "clock failure") || strings.Contains(stderr.String(), "decoder failure") {
				t.Fatalf("code=%d stderr=%s", code, &stderr)
			}
			if clockCalls != 1 || decoderCalls != 1 || policyCalls != 0 {
				t.Fatalf("calls clock=%d decoder=%d policy=%d", clockCalls, decoderCalls, policyCalls)
			}
			regs, err := state.OpenReadOnly(home).LoadRegistrations()
			if err != nil || len(regs) != 0 {
				t.Fatalf("panic admitted registration: %+v %v", regs, err)
			}
		})
	}
}
