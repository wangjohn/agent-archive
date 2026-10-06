package collector

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// Serial: measures the full retained-state decode and byte counters.
func TestUnchangedLabelLookupReusesNarrowRetainedContext(t *testing.T) {
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript)
	reg := registration(t, path)
	reg.NativeSessionID = "01900000-0000-7000-8000-000000000001"
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	now := reg.RegisteredAt.Add(time.Hour)
	provider := &mutableLabels{}
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", Now: func() time.Time { return now }}
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) > 0 {
		t.Fatalf("%+v %v", result, err)
	}
	opts.Labels = mutableLabelLookup{provider}
	var readBytes int64
	opts.labelReadObserver = func(n int64) { readBytes += n }
	now = now.Add(time.Hour)
	before := state.PublishedStateLoads()
	bytesBefore := readBytes
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) > 0 {
		t.Fatalf("%+v %v", result, err)
	}
	if got := state.PublishedStateLoads() - before; got != 1 {
		t.Fatalf("migration full decodes %d", got)
	}
	if got := readBytes - bytesBefore; got <= 0 || got > 16<<20 {
		t.Fatalf("context byte budget %d", got)
	}
	for range 4 {
		now = now.Add(time.Hour)
		before = state.PublishedStateLoads()
		bytesBefore = readBytes
		if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) > 0 {
			t.Fatalf("%+v %v", result, err)
		}
		if state.PublishedStateLoads() != before || readBytes != bytesBefore {
			t.Fatal("unchanged lookup decoded retained conversation again")
		}
	}
}

type fairLabels struct {
	requested map[string]bool
	max       int
}

type fairLabelProvider struct{ *fairLabels }

func (p fairLabelProvider) LookupLabels(_ context.Context, _ agentapi.LabelEnvironment, r []agentapi.LabelRequest) map[string]archive.SessionLabel {
	if len(r) > p.max {
		p.max = len(r)
	}
	for _, req := range r {
		p.requested[req.Registration.ArchiveSessionID] = true
	}
	return nil
}

func (p *fairLabels) LookupLabels(string) (agentapi.LabelProvider, bool) {
	return fairLabelProvider{p}, true
}

func TestLabelLookupCursorDefersFairlyAcrossRestart(t *testing.T) {
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript)
	base := registration(t, path)
	remote := storagetest.NewMemoryStore()
	now := base.RegisteredAt.Add(time.Hour)
	for i := range 130 {
		reg := base
		reg.ArchiveSessionID = fmt.Sprintf("session-%03d", i)
		reg.NativeSessionID = fmt.Sprintf("01900000-0000-7000-8000-%012d", i)
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
	}
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", Now: func() time.Time { return now }}
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) > 0 {
		t.Fatalf("%+v %v", result, err)
	}
	provider := &fairLabels{requested: map[string]bool{}}
	opts.Labels = provider
	for range 3 {
		now = now.Add(time.Hour)
		var err error
		local, err = state.Open(local.Home())
		if err != nil {
			t.Fatal(err)
		}
		if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) > 0 {
			t.Fatalf("%+v %v", result, err)
		}
	}
	if provider.max > 64 || len(provider.requested) != 130 {
		t.Fatalf("max batch %d covered %d", provider.max, len(provider.requested))
	}
}
