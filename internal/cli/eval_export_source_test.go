package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	f := localEvalFixture(t)
	path := filepath.Join(f.root, "resumed.jsonl")
	raw := `{"type":"user","sessionId":"original","message":{"role":"user","content":"old"}}` + "\n" + `{"type":"user","sessionId":"resumed","message":{"role":"user","content":"new"}}` + "\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	records, stderr, code := f.evalLines(t, "", "--file", path, "--harness", "claude")
	if code != 0 || len(records) != 1 || records[0]["session_id"] != "resumed" {
		t.Fatalf("%d %v %s", code, records, stderr)
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

// Backfill admission checks identity consistency, while exported scan identities
// still need the retained records' privacy filtering, just as --file does.
func TestEvalExportScanIdentityComesFromFilteredRecords(t *testing.T) {
	t.Parallel()
	f := localEvalFixture(t)
	secret := "sk-abcdefghijklmnopqrstuv"
	path := filepath.Join(f.userHome, ".claude", "projects", "slug-c-lev-1", secret+".jsonl")
	raw := fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":%q,"timestamp":"2026-09-20T17:00:00Z","message":{"role":"user","content":"inspect"}}`+"\n", secret, filepath.Join(f.userHome, "levenshtein"))
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	for _, detail := range []string{"full", "metadata"} {
		t.Run(detail, func(t *testing.T) {
			t.Parallel()
			direct, stderr, code := f.evalLines(t, "", "--file", path, "--harness", "claude", "--detail", detail)
			if code != 0 || len(direct) != 1 || direct[0]["session_id"] != "[REDACTED]" || direct[0]["native_session_id"] != "[REDACTED]" {
				t.Fatalf("file code %d records %v stderr %s", code, direct, stderr)
			}
			scanned, stderr, code := f.evalLines(t, "", "--scan", "--harness", "claude", "--detail", detail)
			if code != 0 {
				t.Fatalf("scan code %d stderr %s", code, stderr)
			}
			record := recordsBy(scanned, "transcript_path")[path]
			if record == nil || record["session_id"] != direct[0]["session_id"] || record["native_session_id"] != direct[0]["native_session_id"] {
				t.Fatalf("admitted scan identity differs from filtered file identity: %v", record)
			}
		})
	}
}

// Output failure cancels an already-running filter and releases each worker's
// separately owned snapshot and pass before the pool returns.
func TestEvalExportOutputFailureClosesActiveNativeSources(t *testing.T) {
	t.Parallel()
	f := localEvalFixture(t)
	path := filepath.Join(f.userHome, ".claude", "projects", "slug-c-lev-1", "c-lev-1.jsonl")
	p, filter, _ := productionAgents.LookupSources("claude")
	tracked := &evalLifetimeProvider{SourceProvider: p}
	blocking := &evalConcurrentFilter{TranscriptFilter: filter, entered: make(chan struct{})}
	f.env.Agents = evalSourceRegistry(t, "claude", tracked, blocking)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	x := evalExporter{ctx: ctx, env: f.env, opts: evalExportOptions{workers: 2, detail: archive.EvalExportDetailFull}}
	inputs := make([]evalInput, 20)
	for i := range inputs {
		inputs[i] = evalInput{path: path, harness: "claude", given: path}
	}
	if _, err := x.run(inputs, &evalFailWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("output error %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("active filter required external cancellation")
	}
	if !blocking.canceled.Load() || tracked.opens.Load() != 2 || tracked.passesClosed.Load() != 2 || tracked.snapshotsClosed.Load() != 2 {
		t.Fatalf("canceled %v, opens %d, closed passes %d, closed snapshots %d", blocking.canceled.Load(), tracked.opens.Load(), tracked.passesClosed.Load(), tracked.snapshotsClosed.Load())
	}
}

type evalConcurrentFilter struct {
	agentapi.TranscriptFilter
	entered  chan struct{}
	calls    atomic.Int32
	canceled atomic.Bool
}

func (f *evalConcurrentFilter) Filter(ctx context.Context, input agentapi.NativeInput, c agentapi.FilterContext) (archive.FilteredTranscript, error) {
	if f.calls.Add(1) == 1 {
		select {
		case <-f.entered:
		case <-ctx.Done():
			return archive.FilteredTranscript{}, ctx.Err()
		}
		return f.TranscriptFilter.Filter(ctx, input, c)
	}
	close(f.entered)
	<-ctx.Done()
	f.canceled.Store(true)
	return archive.FilteredTranscript{}, ctx.Err()
}

type evalLifetimeProvider struct {
	agentapi.SourceProvider
	opens           atomic.Int32
	passesClosed    atomic.Int32
	snapshotsClosed atomic.Int32
}

func (p *evalLifetimeProvider) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	pass, err := p.SourceProvider.OpenPass(ctx, env)
	if err != nil {
		return nil, err
	}
	p.opens.Add(1)
	return &evalLifetimePass{SourcePass: pass, provider: p}, nil
}

type evalLifetimePass struct {
	agentapi.SourcePass
	provider *evalLifetimeProvider
}

func (p *evalLifetimePass) Read(ctx context.Context, ref agentapi.SourceRef, limits agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	snapshot, err := p.SourcePass.Read(ctx, ref, limits)
	if err != nil {
		return nil, err
	}
	return &evalLifetimeSnapshot{SourceSnapshot: snapshot, provider: p.provider}, nil
}

func (p *evalLifetimePass) Close() error { p.provider.passesClosed.Add(1); return p.SourcePass.Close() }

type evalLifetimeSnapshot struct {
	agentapi.SourceSnapshot
	provider *evalLifetimeProvider
}

func (s *evalLifetimeSnapshot) Close() error {
	s.provider.snapshotsClosed.Add(1)
	return s.SourceSnapshot.Close()
}
