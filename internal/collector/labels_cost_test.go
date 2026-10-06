package collector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

type firstTargetLabels struct {
	attempted []string
	groups    map[string]string
}

type firstTargetProvider struct{ *firstTargetLabels }

func (p firstTargetProvider) LookupLabels(_ context.Context, _ agentapi.LabelEnvironment, requests []agentapi.LabelRequest) map[string]archive.SessionLabel {
	if len(requests) > 0 {
		p.attempted = append(p.attempted, requests[0].Registration.ArchiveSessionID)
	}
	// The first target exhausted the provider's work budget before later targets.
	return nil
}

func (p *firstTargetLabels) LookupLabels(string) (agentapi.LabelProvider, bool) {
	return firstTargetProvider{p}, true
}

func TestLabelBudgetPriorityRotatesSmallBatchesAcrossRestart(t *testing.T) {
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript)
	base := registration(t, path)
	groups := map[string]string{"a": "home-one", "c": "home-one", "f": "home-one", "b": "home-two", "d": "home-two", "e": "home-two"}
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		reg := base
		reg.ArchiveSessionID = id
		reg.NativeSessionID = "native-" + id
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
	}
	now := base.RegisteredAt.Add(time.Hour)
	remote := storagetest.NewMemoryStore()
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", Now: func() time.Time { return now }}
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) > 0 {
		t.Fatalf("%+v %v", result, err)
	}
	provider := &firstTargetLabels{groups: groups}
	opts.Labels = provider
	for range 4 {
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
	if len(provider.attempted) != 4 {
		t.Fatalf("unexpected attempts %v", provider.attempted)
	}
	for i := 1; i < len(provider.attempted); i++ {
		if groups[provider.attempted[i]] == groups[provider.attempted[i-1]] {
			t.Fatalf("budget starved later home across restart: %v", provider.attempted)
		}
	}
}

func (p firstTargetProvider) LabelRequestGroup(_ agentapi.LabelEnvironment, request agentapi.LabelRequest) string {
	sum := sha256.Sum256([]byte(p.groups[request.Registration.ArchiveSessionID]))
	return hex.EncodeToString(sum[:])
}

type rawTargetProvider struct{ firstTargetProvider }

func (p rawTargetProvider) LabelRequestGroup(_ agentapi.LabelEnvironment, request agentapi.LabelRequest) string {
	return p.groups[request.Registration.ArchiveSessionID]
}

func TestLabelBudgetPriorityRotatesDeferredTargetsWithinHomes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ids    []string
		groups map[string]string
		passes int
	}{
		{"contiguous", []string{"a1", "a2", "b1", "b2"}, map[string]string{"a1": "/private/home-one", "a2": "/private/home-one", "b1": "/private/home-two", "b2": "/private/home-two"}, 4},
		{"interleaved", []string{"a1", "b1", "a2", "b2"}, map[string]string{"a1": "one", "a2": "one", "b1": "two", "b2": "two"}, 4},
		{"unequal", []string{"a1", "a2", "b1", "b2", "b3"}, map[string]string{"a1": "one", "a2": "one", "b1": "two", "b2": "two", "b3": "two"}, 6},
		{"single", []string{"a1", "a2", "a3"}, map[string]string{"a1": "one", "a2": "one", "a3": "one"}, 3},
		{"unknown", []string{"a1", "a2", "a3"}, map[string]string{}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := newTestStore(t)
			provider := &firstTargetLabels{groups: tc.groups}
			providers := map[string]agentapi.LabelProvider{"codex": rawTargetProvider{firstTargetProvider{provider}}}
			requests := []agentapi.LabelRequest{}
			for _, id := range tc.ids {
				requests = append(requests, agentapi.LabelRequest{Registration: archive.SessionRegistration{ArchiveSessionID: id, Harness: archive.Harness{Name: "codex"}}})
			}
			cache := state.LabelCache{Version: 1, Entries: map[string]state.LabelEntry{}}
			seen := map[string]bool{}
			for range tc.passes {
				// Every lookup backoff has expired; coverage selects the complete eligible batch.
				ordered := prioritizeLabelRequests(requests, providers, agentapi.LabelEnvironment{}, &cache)
				providers["codex"].LookupLabels(context.Background(), agentapi.LabelEnvironment{}, ordered)
				seen[ordered[0].Registration.ArchiveSessionID] = true
				if err := local.SaveLabels(cache); err != nil {
					t.Fatal(err)
				}
				bytes, err := os.ReadFile(filepath.Join(local.Home(), "session-labels.json"))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(bytes), "/private/home") {
					t.Fatal("raw group persisted")
				}
				local, err = state.Open(local.Home())
				if err != nil {
					t.Fatal(err)
				}
				cache, err = local.LoadLabels()
				if err != nil {
					t.Fatal(err)
				}
			}
			if len(seen) != len(requests) {
				t.Fatalf("available deferred targets starved: attempts=%v", provider.attempted)
			}
		})
	}
}

func TestLabelTargetCursorPrunesRemovedTargetsAndKeepsBackoffProgress(t *testing.T) {
	cache := state.LabelCache{Entries: map[string]state.LabelEntry{"backoff": {NextAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}}, TargetCursors: map[string]string{"present": "backoff", "removed": "gone"}}
	pruneLabelTargetCursors(&cache)
	if len(cache.TargetCursors) != 1 || cache.TargetCursors["present"] != "backoff" {
		t.Fatal("removed cursor survived or deferred progress was lost")
	}
}
