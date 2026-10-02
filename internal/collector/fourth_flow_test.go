package collector

import (
	"bytes"
	"context"
	"encoding/json"
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
	integrations = append(integrations, builtin.Integration{Descriptor: agentmeta.Descriptor{ID: orbifold.ID}, Sources: ports, Filter: ports, Parser: orbifold.Parser{Owner: ports}, Decoder: ports, Imports: ports})
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
	nativeID := "colliding-id"
	ports := &orbifold.Ports{Mutation: agentapi.ReplaceableSnapshot, Generation: 1}
	for i, pulse := range []orbifold.NativePulse{{PulseKind: "exchange", Speaker: "pilot", Words: "Map the orbit", NativeIdentity: nativeID, Clock: now.Format(time.RFC3339), Credential: "private-fixture-only"}, {PulseKind: "exchange", Speaker: "oracle", Words: "The orbit is mapped", NativeIdentity: nativeID, Clock: now.Add(time.Second).Format(time.RFC3339)}} {
		raw, err := json.Marshal(pulse)
		if err != nil {
			t.Fatal(err)
		}
		ports.Shards = append(ports.Shards, raw)
		_ = writeTranscript(t, project, []string{"first.pulse", "second.pulse"}[i], string(raw))
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
	opts := Options{Sources: registry, Parsers: registry, MachineID: "fourth-machine", Now: func() time.Time { return now.Add(time.Hour) }}
	ports.TransientReads = 1
	if _, err := Run(ctx, local, remote, opts); err != nil {
		t.Fatal(err)
	}
	result, err := Run(ctx, local, remote, opts)
	if err != nil || len(result.Errors) > 0 {
		t.Fatalf("retry: %+v %v", result, err)
	}
	metadata := fetchMetadata(t, remote, string(orbifold.ID), reg.ArchiveSessionID)
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
	if _, err := Run(ctx, local, remote, opts); err != nil {
		t.Fatal(err)
	}
	if ports.Reads != reads {
		t.Fatal("parser upgrade reopened native source")
	}
	now = now.Add(time.Hour)
	ports.Shards = ports.Shards[:1]
	ports.Generation++
	result, err = Run(ctx, local, remote, opts)
	if err != nil || len(result.Errors) > 0 {
		t.Fatalf("allowed rewrite: %+v %v", result, err)
	}
	rewritten := fetchMetadata(t, remote, string(orbifold.ID), reg.ArchiveSessionID)
	if rewritten.SourceBundle == metadata.SourceBundle || len(rewritten.CaptureGaps) == 0 {
		t.Fatalf("replacement provenance absent: %+v", rewritten)
	}
}
