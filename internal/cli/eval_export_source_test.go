package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// A provider failure can contain native data; export must retain only the error code.
func TestEvalExportLocalErrorsOmitNativeDetails(t *testing.T) {
	t.Parallel()
	f := localEvalFixture(t)
	path := filepath.Join(f.userHome, ".claude", "projects", "slug-c-lev-1", "c-lev-1.jsonl")
	p, filter, _ := productionAgents.LookupSources("claude")
	f.env.Agents = evalSourceRegistry(t, "claude", p, evalErrorFilter{TranscriptFilter: filter})
	records, stderr, code := f.evalLines(t, "", "--file", path, "--harness", "claude")
	if code != 1 || len(records) != 1 || records[0]["error"].(map[string]any)["code"] != "read_failed" || strings.Contains(records[0]["error"].(map[string]any)["message"].(string)+stderr, "private-synthetic-source-secret") {
		t.Fatalf("code %d, records %v, stderr %s", code, records, stderr)
	}
}

type evalErrorFilter struct{ agentapi.TranscriptFilter }

func (f evalErrorFilter) Filter(context.Context, agentapi.NativeInput, agentapi.FilterContext) (archive.FilteredTranscript, error) {
	return archive.FilteredTranscript{}, errors.New("private-synthetic-source-secret")
}

func evalSourceRegistry(t *testing.T, name string, provider agentapi.SourceProvider, filter agentapi.TranscriptFilter) *builtin.Registry {
	t.Helper()
	var bindings []builtin.Integration
	for _, d := range productionAgents.Catalog().All() {
		b, _ := productionAgents.Lookup(string(d.ID))
		b.Descriptor = agentmeta.Descriptor{ID: d.ID}
		if string(d.ID) == name {
			b.Sources, b.Filter = provider, filter
		}
		bindings = append(bindings, b)
	}
	registry, err := builtin.New(agentmeta.Builtins(), bindings)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

// With one worker a failed output must stop before opening the next source.
func TestEvalExportOutputFailureStopsNativeReads(t *testing.T) {
	t.Parallel()
	f := localEvalFixture(t)
	path := filepath.Join(f.userHome, ".claude", "projects", "slug-c-lev-1", "c-lev-1.jsonl")
	p, filter, _ := productionAgents.LookupSources("claude")
	counter := &evalCountingProvider{SourceProvider: p}
	f.env.Agents = evalSourceRegistry(t, "claude", counter, filter)
	x := evalExporter{ctx: context.Background(), env: f.env, opts: evalExportOptions{workers: 1, detail: archive.EvalExportDetailFull}}
	inputs := make([]evalInput, 10)
	for i := range inputs {
		inputs[i] = evalInput{path: path, harness: "claude", given: path}
	}
	if _, err := x.run(inputs, &evalFailWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	if counter.opens != 1 {
		t.Fatalf("opened %d sources after first output failed", counter.opens)
	}
}

type evalCountingProvider struct {
	agentapi.SourceProvider
	opens int
}

func (p *evalCountingProvider) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	p.opens++
	return p.SourceProvider.OpenPass(ctx, env)
}

// session_meta precedes the first prompt, and its original start must survive export.
func TestEvalExportLocalUsesNativeSessionStart(t *testing.T) {
	t.Parallel()
	f := localEvalFixture(t)
	path := filepath.Join(f.root, "native.jsonl")
	raw := `{"type":"session_meta","timestamp":"2026-09-20T17:00:03Z","payload":{"id":"native-start","timestamp":"2026-09-20T17:00:00Z","cwd":"/work/widget"}}` + "\n" + `{"type":"response_item","timestamp":"2026-09-20T17:00:05Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"inspect"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	for _, detail := range []string{"full", "metadata"} {
		records, stderr, code := f.evalLines(t, "", "--file", path, "--harness", "codex", "--detail", detail)
		if code != 0 || len(records) != 1 || (records[0]["started_at"] != "2026-09-20T17:00:00Z" || records[0]["session_id"] != "native-start") {
			t.Fatalf("%s: code %d records %v stderr %s", detail, code, records, stderr)
		}
	}
}

// Copied Claude history can name its old session first; the admitted file stem wins.
func TestEvalExportLocalUsesResumedClaudeIdentity(t *testing.T) {
	t.Parallel()
	filtered := archive.FilteredTranscript{SessionIDs: []string{"original", "resumed"}, Records: [][]byte{[]byte(`{"sessionId":"original"}`), []byte(`{"sessionId":"resumed"}`)}}
	if got := transcriptSessionID("claude", "/sessions/resumed.jsonl", filtered); got != "resumed" {
		t.Fatalf("identity %s", got)
	}
}

// A canceled pool never opens a native provider.
func TestEvalExportCanceledPoolDoesNotRead(t *testing.T) {
	t.Parallel()
	f := localEvalFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	x := evalExporter{ctx: ctx, env: f.env, opts: evalExportOptions{workers: 2}}
	if _, err := x.run([]evalInput{{path: "/not-opened", harness: "claude"}}, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v", err)
	}
}

// A malformed or failed input stream is rejected before any record is emitted.
func TestEvalExportIDsFromReadFailureProducesNoRecords(t *testing.T) {
	t.Parallel()
	f := localEvalFixture(t)
	records, stderr, code := f.evalLines(t, strings.Repeat("x", maxEvalInputLine+1), "--ids-from", "-")
	if code != 1 || len(records) != 0 || !strings.Contains(stderr, "read --ids-from") {
		t.Fatalf("code %d records %v stderr %s", code, records, stderr)
	}
}

// Filtering retains raw IDs for identity checks; exported IDs must use the safe records.
func TestEvalExportLocalIdentityComesFromFilteredRecords(t *testing.T) {
	t.Parallel()
	f := localEvalFixture(t)
	path := filepath.Join(f.root, "safe-name.jsonl")
	raw := `{"type":"user","sessionId":"sk-abcdefghijklmnopqrstuv","timestamp":"2026-09-20T17:00:00Z","message":{"role":"user","content":"inspect"}}` + "\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	records, stderr, code := f.evalLines(t, "", "--file", path, "--harness", "claude")
	if code != 0 || len(records) != 1 || strings.Contains(records[0]["session_id"].(string), "sk-abcdefghijklmnopqrstuv") {
		t.Fatalf("code %d records %v stderr %s", code, records, stderr)
	}
}
