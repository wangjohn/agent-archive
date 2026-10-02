package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/agentskills"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/storage"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"github.com/wangjohn/agent-archive/internal/testutil/agenttest"
	"github.com/wangjohn/agent-archive/internal/testutil/orbifold"
)

func fourthRegistry(t *testing.T, ports *orbifold.Ports) *builtin.Registry {
	t.Helper()
	base := builtin.NewBuiltins()
	descriptors := base.Catalog().All()
	var integrations []builtin.Integration
	for _, d := range descriptors {
		integration, _ := base.Lookup(string(d.ID))
		integration.Descriptor = agentmeta.Descriptor{ID: d.ID}
		integrations = append(integrations, integration)
		d.Operations = nil
	}
	descriptors = append(descriptors, agentmeta.Descriptor{ID: orbifold.ID, DisplayName: "Orbifold fixture"})
	catalog, err := agentmeta.New(descriptors)
	if err != nil {
		t.Fatal(err)
	}
	integrations = append(integrations, builtin.Integration{Descriptor: agentmeta.Descriptor{ID: orbifold.ID}, Sources: ports, Filter: ports, Parser: orbifold.Parser{Owner: ports}, Decoder: ports, Imports: ports, Hooks: orbifold.Hooks{}, Skills: orbifold.Skills(), Launcher: orbifold.Launcher{}, Runtime: ports, Version: ports, Evidence: ports, NativeHeaders: orbifold.Discovery{Qualified: true}, Discovery: orbifold.Discovery{Qualified: true}, NativePaths: ports, Preview: ports})
	registry, err := builtin.New(catalog, integrations)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestFourthNormalRegistryFlow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, project := t.TempDir(), t.TempDir()
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	nativeID := " collision/λ\x00 "
	ports := &orbifold.Ports{Mutation: agentapi.ReplaceableSnapshot, Generation: 1}
	for i, pulse := range []orbifold.NativePulse{{PulseKind: "exchange", Speaker: "pilot", Words: "Map the orbit", NativeIdentity: nativeID, Clock: now.Format(time.RFC3339), Credential: "private-fixture-only"}, {PulseKind: "exchange", Speaker: "oracle", Words: "The orbit is mapped", NativeIdentity: nativeID, Clock: now.Add(time.Second).Format(time.RFC3339)}} {
		raw, err := json.Marshal(pulse)
		if err != nil {
			t.Fatal(err)
		}
		ports.Shards = append(ports.Shards, raw)
		ports.ShardPaths = append(ports.ShardPaths, writeFourthTranscript(t, project, []string{"first.pulse", "second.pulse"}[i], string(raw)))
	}
	registry := fourthRegistry(t, ports)
	cfg := config.Config{MachineID: "fourth-machine", Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "test-bucket", Region: "us-east-1", AWSProfile: "test", Prefix: "agent-archive/"}, Archive: archive.Config{SchemaVersion: 1, MachineID: "fourth-machine", Enabled: true, Projects: []archive.ProjectActivation{{ProjectID: archive.ProjectID(project), Root: project, Included: true, ActivatedAt: now.Add(-time.Hour)}}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	decoder, _ := registry.LookupDecoder(string(orbifold.ID))
	emit := func(pulse string) {
		t.Helper()
		batch, err := decoder.Decode(ctx, agentapi.HookInput{ObservedAt: now, Payload: map[string]any{"opaque": nativeID, "checkout": project, "manifest": project + "/manifest.orbit", "pulse": pulse}})
		if err != nil {
			t.Fatal(err)
		}
		if err := capture.HandleBatch(home, string(orbifold.ID), batch, now); err != nil {
			t.Fatal(err)
		}
	}
	emit("resume")
	local, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	regs, err := local.LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatalf("resume admitted: %+v %v", regs, err)
	}
	emit("birth")
	emit("birth")
	emit("reply")
	emit("rest")
	regs, err = local.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("replay admission: %+v %v", regs, err)
	}
	reg := regs[0]
	builtinCollision := reg
	builtinCollision.ArchiveSessionID = "builtin-collision"
	builtinCollision.Harness.Name = "claude"
	builtinCollision.TranscriptPath = ""
	if err := local.SaveRegistration(builtinCollision); err != nil {
		t.Fatal(err)
	}
	key, _ := agentmeta.NewSessionKey(string(orbifold.ID), nativeID)
	id, found, err := local.ArchiveSessionID(key)
	if err != nil || !found || id != reg.ArchiveSessionID {
		t.Fatalf("qualified identity: %q %v %v", id, found, err)
	}
	remote := storagetest.NewMemoryStore()
	opts := collector.Options{Sources: registry, Parsers: registry, MachineID: "fourth-machine", Now: func() time.Time { return now.Add(time.Hour) }}
	ports.TransientReads = 1
	if _, err := collector.Run(ctx, local, remote, opts); err != nil {
		t.Fatal(err)
	}
	result, err := collector.Run(ctx, local, remote, opts)
	if err != nil || len(result.Errors) > 0 {
		t.Fatalf("retry: %+v %v", result, err)
	}
	metadata := fetchFourthMetadata(t, remote, string(orbifold.ID), reg.ArchiveSessionID)
	bundle, err := reader.LoadSource(ctx, remote, metadata, reader.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.NativeRecords) != 2 || bundle.Capture.SourceFormat != orbifold.Format {
		t.Fatalf("native record envelope: %+v", bundle)
	}
	encoded, _ := json.Marshal(bundle)
	if bytes.Contains(encoded, []byte("credential")) || bytes.Contains(encoded, []byte("private-fixture-only")) {
		t.Fatal("native privacy field escaped")
	}
	parser, _ := registry.LookupParser(string(orbifold.ID))
	analysis, err := agentapi.Analyze(ctx, parser, bundle)
	if err != nil {
		t.Fatal(err)
	}
	handoff, err := archive.BuildHandoffWithAnalysis(bundle, analysis, &metadata, archive.HandoffOptions{})
	if err != nil || len(handoff.Exchanges) != 1 || !handoff.ToolResultsUnavailable {
		t.Fatalf("handoff: %+v %v", handoff, err)
	}
	if metadata.Counts.Compactions != nil || metadata.Counts.ToolErrors != nil {
		t.Fatalf("unavailable metrics fabricated: %+v", metadata.Counts)
	}
	keys, err := reader.NewMetadataFinder(registry.Catalog()).FindMetadataKeys(ctx, remote, "", reg.ArchiveSessionID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("normal discovery: %v %v", keys, err)
	}
	reads := ports.Reads
	ports.ParserVersion = "orbifold-parser-2"
	if _, err := collector.Run(ctx, local, remote, opts); err != nil {
		t.Fatal(err)
	}
	if ports.Reads != reads {
		t.Fatal("parser upgrade reopened native source")
	}
	now = now.Add(time.Hour)
	ports.Shards = ports.Shards[:1]
	ports.ShardPaths = ports.ShardPaths[:1]
	ports.Generation++
	result, err = collector.Run(ctx, local, remote, opts)
	if err != nil || len(result.Errors) > 0 {
		t.Fatalf("allowed rewrite: %+v %v", result, err)
	}
	rewritten := fetchFourthMetadata(t, remote, string(orbifold.ID), reg.ArchiveSessionID)
	if rewritten.SourceBundle == metadata.SourceBundle || len(rewritten.CaptureGaps) == 0 {
		t.Fatalf("replacement provenance absent: %+v", rewritten)
	}
}

func writeFourthTranscript(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func fetchFourthMetadata(t *testing.T, store storage.ObjectStore, name, id string) archive.Metadata {
	t.Helper()
	key, err := archive.MetadataObjectKey(name, id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var m archive.Metadata
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestFourthHistoricalQualifiedLocatorFlow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	ports := &orbifold.Ports{Generation: 1}
	registry := fourthRegistry(t, ports)
	root := filepath.Join(userHome, ".orbifold", "constellations")
	must(t, os.MkdirAll(root, 0700))
	pulse := orbifold.NativePulse{PulseKind: "exchange", Speaker: "pilot", Words: "Historical orbit", NativeIdentity: "historical", Clock: at.Add(-time.Hour).Format(time.RFC3339), Landing: project}
	raw, err := json.Marshal(pulse)
	must(t, err)
	ports.Shards = [][]byte{raw}
	manifest := writeFourthTranscript(t, root, "orbit-historical.orbit", string(raw)+"\n")
	cfg := config.Config{MachineID: "fourth", Harnesses: []string{"claude"}, Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "test-bucket", Region: "us-east-1", AWSProfile: "test"}, Archive: archive.Config{SchemaVersion: 1, Enabled: true, MachineID: "fourth", Projects: []archive.ProjectActivation{{ProjectID: archive.ProjectID(project), Root: project, Included: true, ActivatedAt: at.Add(-2 * time.Hour)}}}}
	must(t, config.Save(home, cfg))
	local, err := state.Open(home)
	must(t, err)
	env := backfill.Environment{Home: userHome, Discovery: registry, Sources: registry, Imports: registry, NativePaths: registry, Now: func() time.Time { return at }, CursorDatabase: func(context.Context) (backfill.CursorDatabaseResult, error) {
		return backfill.CursorDatabaseResult{Checked: true}, nil
	}}
	plan, err := backfill.BuildPlan(ctx, env, newArchiveState(home, cfg), cfg, backfill.Filters{Harnesses: []string{string(orbifold.ID)}})
	must(t, err)
	imported := plan.Imported()
	if len(imported) != 1 || imported[0].SourceKind != orbifold.Kind || imported[0].SourceKey != "historical" || imported[0].TranscriptPath != manifest {
		t.Fatalf("qualified discovery lost: %+v", plan.Candidates)
	}
	_, err = backfill.ApplyToConfig(&cfg, plan, at)
	must(t, err)
	must(t, config.Save(home, cfg))
	registration := backfill.Registration{Home: home, Store: local, Batch: "2026-09-01-1", AdmittedAt: at, DestinationID: cfg.DestinationID()}
	result, err := registration.Run(imported)
	if err != nil || len(result.Sessions) != 1 {
		t.Fatalf("historical admission: %+v %v", result, err)
	}
	remote := storagetest.NewMemoryStore()
	collected, err := collector.Run(ctx, local, remote, collector.Options{Sources: registry, Parsers: registry, MachineID: "fourth", Now: func() time.Time { return at.Add(time.Hour) }})
	if err != nil || len(collected.Published) != 1 || len(collected.Errors) != 0 {
		t.Fatalf("historical publication: %+v %v", collected, err)
	}
	metadata := fetchFourthMetadata(t, remote, string(orbifold.ID), result.Sessions[0])
	bundle, err := reader.LoadSource(ctx, remote, metadata, reader.Limits{})
	must(t, err)
	if bundle.NativeSessionID != "historical" || bundle.Capture.SourceFormat != orbifold.Format {
		t.Fatalf("historical readback: %+v", bundle)
	}
	replay, err := backfill.BuildPlan(ctx, env, newArchiveState(home, cfg), cfg, backfill.Filters{Harnesses: []string{string(orbifold.ID)}})
	must(t, err)
	if len(replay.Imported()) != 0 || replay.Candidates[0].Skip != backfill.SkipAlreadyArchived {
		t.Fatalf("historical replay: %+v", replay.Candidates)
	}
}

func TestFourthSetupJournalSkillsAndRemoval(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	ports := &orbifold.Ports{}
	registry := fourthRegistry(t, ports)
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	env.Agents = registry
	executable, err := env.executable()
	must(t, err)
	location := (orbifold.Hooks{}).Location(agentapi.HookLocations{UserHome: userHome})
	must(t, os.MkdirAll(filepath.Dir(location), 0700))
	foreign := []byte(`{"color":"ultraviolet","orbits":{"/other/owner":{"birth":"foreign"}}}`)
	must(t, os.WriteFile(location, foreign, 0600))
	next := config.Config{MachineID: "fourth", Harnesses: []string{string(orbifold.ID)}, Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "test-bucket", Region: "us-east-1", AWSProfile: "test"}, Archive: archive.Config{SchemaVersion: 1, MachineID: "fourth", Enabled: true}}
	must(t, applySetup(home, userHome, executable, config.Config{}, &next, nil, env))
	if next.HookFiles[string(orbifold.ID)] != location {
		t.Fatalf("native setup path: %v", next.HookFiles)
	}
	installed := agentskills.Installed(registry, userHome, "", home)
	if len(installed) == 0 {
		t.Fatal("shared templates were not installed")
	}
	health := statusView{Apps: []appStatus{{Name: string(orbifold.ID)}}}
	readInstalledApps(&health, next, home, userHome, env)
	if health.Apps[0].Hooks != "installed" {
		t.Fatalf("native health inspector: %+v", health.Apps)
	}
	changes, found, err := hooks.PlanRemovalOf(env.installedHookFiles(userHome, next), env.installation(home, userHome).owner(), string(orbifold.ID))
	must(t, err)
	if !found {
		t.Fatal("native hook removal absent")
	}
	must(t, hooks.Apply([]hooks.Change{changes}))
	raw, err := os.ReadFile(location)
	must(t, err)
	if !bytes.Contains(raw, []byte("ultraviolet")) || !bytes.Contains(raw, []byte("foreign")) || bytes.Contains(raw, []byte(executable)) {
		t.Fatalf("foreign hook ownership changed: %s", raw)
	}
	removal, kept, err := agentskills.PlanRemoval(registry, userHome, "", home)
	must(t, err)
	if len(kept) != 0 || len(removal) == 0 {
		t.Fatalf("native skill removal: %v %v", removal, kept)
	}
	must(t, hooks.Apply(removal))
	if installed := agentskills.Installed(registry, userHome, "", home); len(installed) != 0 {
		t.Fatalf("owned skill files remain: %v", installed)
	}
}

func TestFourthRecordOwnershipConformance(t *testing.T) {
	t.Parallel()
	ports := &orbifold.Ports{Generation: 1, Shards: [][]byte{[]byte(`{"pulseKind":"exchange"}`), []byte(`{"pulseKind":"exchange"}`)}}
	agenttest.RecordSource(t, ports, agentapi.SourceRef{Kind: orbifold.Kind, Key: "fixture"}, agentapi.SourceEnvironment{})
}
