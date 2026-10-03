package cli

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
)

type evalAnalysisParser struct {
	calls   atomic.Int32
	failure error
}

func (*evalAnalysisParser) Version() string { return "synthetic-7" }

func (p *evalAnalysisParser) Parse(ctx context.Context, _ archive.SourceBundle) (archive.Analysis, error) {
	p.calls.Add(1)
	if p.failure != nil {
		return archive.Analysis{}, p.failure
	}
	if err := ctx.Err(); err != nil {
		return archive.Analysis{}, err
	}
	return archive.Analysis{
		Facts: archive.NativeFacts{FirstBranch: "initial", WorkspaceRoot: "/work/synthetic", NativeStartedAt: time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)},
		View: archive.NormalizedView{Turns: []archive.NormalizedTurn{
			{Kind: archive.TurnKindHumanPrompt, Text: "native framing", PresentationText: "synthetic prompt", PresentationKnown: true},
			{Kind: archive.TurnKindAssistant, Text: "synthetic reply"},
		}},
		Observability: archive.Observability{StructuredCounts: archive.Availability{State: archive.AvailabilityAvailable}},
	}, nil
}

type evalSyntheticFilter struct{ agentapi.TranscriptFilter }

func (evalSyntheticFilter) Name() string { return string(syntheticID) }

func evalAnalysisRegistry(t *testing.T, parser agentapi.TranscriptParser) *builtin.Registry {
	t.Helper()
	descriptors := agentmeta.Builtins().All()
	descriptors = append(descriptors, agentmeta.Descriptor{ID: syntheticID, DisplayName: "Synthetic"})
	catalog, err := agentmeta.New(descriptors)
	if err != nil {
		t.Fatal(err)
	}
	var bindings []builtin.Integration
	for _, d := range productionAgents.Catalog().All() {
		b, _ := productionAgents.Lookup(string(d.ID))
		b.Descriptor = agentmeta.Descriptor{ID: d.ID}
		bindings = append(bindings, b)
	}
	provider, filter, _ := productionAgents.LookupSources("claude")
	bindings = append(bindings, builtin.Integration{Descriptor: agentmeta.Descriptor{ID: syntheticID}, Sources: provider, Filter: evalSyntheticFilter{filter}, Parser: parser})
	registry, err := builtin.New(catalog, bindings)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

// A fourth integration's typed parser owns both local export derivation and full archived export.
func TestEvalExportUsesInjectedAnalysisForLocalAndArchive(t *testing.T) {
	t.Parallel()
	parser := &evalAnalysisParser{}
	registry := evalAnalysisRegistry(t, parser)
	f := localEvalFixture(t)
	f.env.Agents = registry
	path := filepath.Join(f.userHome, ".claude", "projects", "slug-c-lev-1", "c-lev-1.jsonl")
	for _, detail := range []string{"full", "metadata"} {
		records, stderr, code := f.evalLines(t, "", "--file", path, "--harness", "synthetic", "--detail", detail)
		if code != 0 || len(records) != 1 {
			t.Fatalf("local %s: %d %v %s", detail, code, records, stderr)
		}
		record := records[0]
		if record["branch"] != "initial" || record["started_at"] != "2026-09-20T10:00:00Z" || record["parser"].(map[string]any)["version"] != "synthetic-7" {
			t.Fatalf("local: %v", record)
		}
		if detail == "full" && record["prompts"].([]any)[0].(map[string]any)["text"] != "synthetic prompt" {
			t.Fatalf("prompts: %v", record)
		}
	}
	env, store, id := publishedFixture(t)
	key, _ := archive.MetadataObjectKey("codex", id)
	metadata, bundle, err := reader.RefreshAndLoad(context.Background(), store, key, reader.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	bundle.Capture.Harness.Name, bundle.Capture.AdapterName = string(syntheticID), string(syntheticID)
	compressed, err := archive.BuildCompressedSource(bundle)
	if err != nil {
		t.Fatal(err)
	}
	sourceKey, err := archive.SourceObjectKey(bundle, compressed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	metadata.Harness.Name, metadata.Adapter.Name = string(syntheticID), string(syntheticID)
	metadata.SourceBundle = archive.SourceReference{Key: sourceKey, SHA256: compressed.SHA256, CompressedBytes: len(compressed.Bytes)}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	key, _ = archive.MetadataObjectKey("synthetic", id)
	if err := store.Put(context.Background(), sourceKey, compressed.Bytes); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), key, raw); err != nil {
		t.Fatal(err)
	}
	env.Agents = registry
	records, stderr, code := evalLines(t, env, "--harness", "synthetic", id)
	if code != 0 || len(records) != 1 || records[0]["prompts"].([]any)[0].(map[string]any)["text"] != "synthetic prompt" {
		t.Fatalf("archive: %d %v %s", code, records, stderr)
	}
	if got := records[0]["parser"].(map[string]any)["version"]; got != parser.Version() {
		t.Fatalf("archive parser version = %v, want %s", got, parser.Version())
	}
	if got := records[0]["counts"].(map[string]any)["turns"]; got != float64(1) {
		t.Fatalf("archive turns = %v, want the injected analysis's one prompt", got)
	}
	if parser.calls.Load() != 3 {
		t.Fatalf("parser called %d times, want once per derivation", parser.calls.Load())
	}
	// Sidecar-only output needs no native parser even for an injected integration.
	records, _, code = evalLines(t, env, "--harness", "synthetic", "--detail", "metadata", id)
	if code != 0 || len(records) != 1 || parser.calls.Load() != 3 {
		t.Fatalf("metadata: %d %v calls %d", code, records, parser.calls.Load())
	}
}

// Parser failures do not expose private native error text or emit empty full records.
func TestEvalExportInjectedParseFailureOmitsNativeDetails(t *testing.T) {
	t.Parallel()
	f := localEvalFixture(t)
	f.env.Agents = evalAnalysisRegistry(t, &evalAnalysisParser{failure: errors.New("private-parser-content")})
	path := filepath.Join(f.userHome, ".claude", "projects", "slug-c-lev-1", "c-lev-1.jsonl")
	records, stderr, code := f.evalLines(t, "", "--file", path, "--harness", "synthetic")
	if code != 1 || len(records) != 1 || records[0]["error"].(map[string]any)["code"] != "parse_failed" || stderr != "" {
		t.Fatalf("%d %v %s", code, records, stderr)
	}
	if records[0]["error"].(map[string]any)["message"] != "the filtered transcript could not be parsed" {
		t.Fatalf("error: %v", records)
	}
}
