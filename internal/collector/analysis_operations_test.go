package collector

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type operationParser struct {
	calls   int
	version string
	failure error
}

func (p *operationParser) Version() string { return p.version }

func (p *operationParser) Parse(ctx context.Context, b archive.SourceBundle) (archive.Analysis, error) {
	p.calls++
	if p.failure != nil {
		return archive.Analysis{}, p.failure
	}
	return (codex.Parser{}).Parse(ctx, b)
}

type operationBindings struct {
	parser  *operationParser
	filter  *operationFilter
	lookups int
}

func (b *operationBindings) LookupParser(name string) (agentapi.TranscriptParser, bool) {
	b.lookups++
	return b.parser, name == "codex"
}

func (b *operationBindings) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	return codex.SourceProvider{}, b.filter, name == "codex"
}

type countedCodexFilter = codex.Filter

type operationFilter struct {
	countedCodexFilter
	calls     int
	refilters int
}

func (f *operationFilter) Filter(ctx context.Context, in agentapi.NativeInput, c agentapi.FilterContext) (archive.FilteredTranscript, error) {
	f.calls++
	return f.countedCodexFilter.Filter(ctx, in, c)
}

func (f *operationFilter) Refilter(ctx context.Context, b archive.SourceBundle, at time.Time) (archive.FilteredTranscript, error) {
	f.refilters++
	return f.countedCodexFilter.Refilter(ctx, b, at)
}

func TestInjectedParserUpgradeReadsRetainedSourceOnce(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript)
	reg := registration(t, path)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	bindings := &operationBindings{parser: &operationParser{version: "first"}, filter: &operationFilter{}}
	remote := storagetest.NewMemoryStore()
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: bindings, Parsers: bindings, MachineID: "machine", Now: func() time.Time { return now }}
	run := func() {
		t.Helper()
		result, err := Run(context.Background(), local, remote, opts)
		if err != nil || len(result.Errors) != 0 {
			t.Fatalf("run: %#v %v", result, err)
		}
	}
	run()
	first := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	retained, err := remote.Get(context.Background(), first.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	if bindings.lookups != 1 || bindings.parser.calls != 1 || bindings.filter.calls != 1 {
		t.Fatalf("initial lookup/parse/filter: %d/%d/%d", bindings.lookups, bindings.parser.calls, bindings.filter.calls)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	bindings.parser.version = "second"
	bindings.parser.calls = 0
	bindings.lookups = 0
	bindings.filter.calls = 0
	now = now.Add(time.Hour)
	run()
	next := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	after, err := remote.Get(context.Background(), first.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	if next.Parser.Version != "second" || next.SourceBundle != first.SourceBundle || !bytes.Equal(retained, after) {
		t.Fatalf("retained source changed: %#v", next)
	}
	if bindings.lookups != 1 || bindings.parser.calls != 1 || bindings.filter.calls != 0 || bindings.filter.refilters != 0 {
		t.Fatalf("refresh lookup/parse/filter/refilter: %d/%d/%d/%d", bindings.lookups, bindings.parser.calls, bindings.filter.calls, bindings.filter.refilters)
	}
}

func TestInjectedParseFailureRetainsSafeSourceAndUnknownCounts(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	bindings := &operationBindings{parser: &operationParser{version: "failed", failure: errors.New("synthetic parser failure")}, filter: &operationFilter{}}
	remote := storagetest.NewMemoryStore()
	result, err := Run(context.Background(), local, remote, Options{Sources: bindings, Parsers: bindings, MachineID: "machine", RequireSkillUse: true, Now: func() time.Time { return reg.RegisteredAt.Add(time.Hour) }})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("run: %#v %v", result, err)
	}
	metadata := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if metadata.Parser.Status != archive.ParserStatusFailed || metadata.Counts.Turns != nil || metadata.Counts.Compactions != nil || metadata.Counts.ToolErrors != nil {
		t.Fatalf("failed parse fabricated observations: %#v", metadata)
	}
	source, err := remote.Get(context.Background(), metadata.SourceBundle.Key)
	if err != nil || len(source) == 0 {
		t.Fatalf("safe source missing: %v", err)
	}
	if bindings.lookups != 1 || bindings.parser.calls != 1 || bindings.filter.calls != 1 {
		t.Fatalf("lookup/parse/filter: %d/%d/%d", bindings.lookups, bindings.parser.calls, bindings.filter.calls)
	}
}
