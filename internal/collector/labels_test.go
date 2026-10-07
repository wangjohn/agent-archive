package collector

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type mutableLabels struct {
	label archive.SessionLabel
	calls int
}

func (p *mutableLabels) LookupLabelsNative(_ context.Context, _ agentapi.LabelEnvironment, requests []agentapi.LabelRequest) map[string]archive.SessionLabel {
	p.calls++
	out := map[string]archive.SessionLabel{}
	for _, r := range requests {
		if p.label.State != "" {
			l := p.label
			l.NativeID = r.Registration.NativeSessionID
			out[r.Registration.ArchiveSessionID] = l
		}
	}
	return out
}

type mutableLabelProvider struct{ *mutableLabels }

func (p mutableLabelProvider) LookupLabels(ctx context.Context, env agentapi.LabelEnvironment, r []agentapi.LabelRequest) map[string]archive.SessionLabel {
	return p.LookupLabelsNative(ctx, env, r)
}

type mutableLabelLookup struct{ *mutableLabels }

func (p mutableLabelLookup) LookupLabels(string) (agentapi.LabelProvider, bool) {
	return mutableLabelProvider(p), true
}

func TestExternalRenamePublishesRetainedSourceWithoutReadingTranscript(t *testing.T) {
	for _, tc := range []struct {
		name    string
		missing bool
		upgrade bool
	}{{name: "unchanged"}, {name: "rotated", missing: true}, {name: "filter upgrade", upgrade: true}} {
		t.Run(tc.name, func(t *testing.T) {
			local := newTestStore(t)
			path := writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript)
			reg := registration(t, path)
			reg.NativeSessionID = "01900000-0000-7000-8000-000000000001"
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			filter := &operationFilter{}
			bindings := &operationBindings{parser: &operationParser{version: "0.1.0"}, filter: filter}
			remote := &countedPublications{MemoryStore: storagetest.NewMemoryStore()}
			now := reg.RegisteredAt.Add(time.Hour)
			provider := &mutableLabels{}
			opts := Options{Sources: bindings, Parsers: bindings, Labels: mutableLabelLookup{provider}, MachineID: "machine", Now: func() time.Time { return now }}
			result, err := Run(context.Background(), local, remote, opts)
			if err != nil || len(result.Errors) > 0 {
				t.Fatalf("%+v %v", result, err)
			}
			before := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
			beforeBundle := fetchBundle(t, remote, before)
			if tc.upgrade {
				simulateFilterUpgrade(t, local)
			}
			if tc.missing {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			now = now.Add(time.Hour)
			provider.label = archive.SessionLabel{State: archive.SessionLabelPresent, Name: "Invented native rename", Source: archive.SessionLabelDatabase, Contract: codex.LabelContract}
			filter.calls = 0
			remote.keys = nil
			result, err = Run(context.Background(), local, remote, opts)
			if err != nil || len(result.Errors) > 0 || len(result.Published) != 1 {
				t.Fatalf("%+v %v", result, err)
			}
			if tc.upgrade {
				// The stale-filter fixture intentionally disagrees with its
				// acknowledged sidecar. Ordinary refiltering repairs that state
				// before it may certify a native label lookup.
				repaired := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
				if provider.calls != 0 || repaired.Name != before.Name || repaired.SourceBundle != before.SourceBundle || !repaired.CapturedAt.Equal(before.CapturedAt) {
					t.Fatal("stale-filter state authorized a native label")
				}
				published, err := local.LoadPublishedState(reg.ArchiveSessionID)
				if err != nil {
					t.Fatal(err)
				}
				if _, found, err := published.LastPublishedMetadata(); err != nil || !found {
					t.Fatal("ordinary refilter did not restore acknowledged authority", err)
				}
				now = now.Add(time.Hour)
				result, err = Run(context.Background(), local, remote, opts)
				if err != nil || len(result.Errors) > 0 || len(result.Published) != 1 {
					t.Fatalf("rename after authority repair: %+v %v", result, err)
				}
			}
			after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
			afterBundle := fetchBundle(t, remote, after)
			encoded, err := archive.BuildCompressedSource(afterBundle)
			if err != nil || encoded.SHA256 != after.SourceBundle.SHA256 || len(encoded.Bytes) != after.SourceBundle.CompressedBytes || after.MachineID != opts.MachineID || after.NativeSessionID != reg.NativeSessionID || after.FilterVersion != archive.FilterVersion || after.SchemaVersion != archive.MetadataSchemaVersion || after.History != nil {
				t.Fatal("rename did not preserve supported ordinary publication authority", err)
			}
			if !tc.upgrade && filter.calls != 0 {
				t.Fatalf("name-only refresh filtered native input %d times", filter.calls)
			}
			if tc.upgrade && filter.calls != 1 {
				t.Fatalf("codec upgrade did not normally refilter native input: %d", filter.calls)
			}
			if after.Name != "Invented native rename" || after.Title != before.Title || after.SourceBundle == before.SourceBundle || !after.CapturedAt.Equal(before.CapturedAt) || !after.StartedAt.Equal(before.StartedAt) || !reflect.DeepEqual(after.EndedAt, before.EndedAt) || !reflect.DeepEqual(before.Counts, after.Counts) || !reflect.DeepEqual(beforeBundle.NativeRecords, afterBundle.NativeRecords) {
				t.Fatalf("rename changed conversation facts: before=%+v after=%+v", before, after)
			}
			now = now.Add(time.Hour)
			provider.label = archive.SessionLabel{}
			remote.keys = nil
			result, err = Run(context.Background(), local, remote, opts)
			if err != nil || len(result.Errors) > 0 || len(remote.keys) != 0 {
				t.Fatalf("unavailable republished: %+v %v %v", result, err, remote.keys)
			}
			if got := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID); got.Name != after.Name {
				t.Fatal("unavailable cleared last good name")
			}
			now = now.Add(time.Hour)
			provider.label = archive.SessionLabel{State: archive.SessionLabelPresent, Name: " \n    <external_codex_apps_open_page>example</external_codex_apps_open_page>", Source: archive.SessionLabelDatabase, Contract: codex.LabelContract}
			result, err = Run(context.Background(), local, remote, opts)
			if err != nil || len(result.Errors) > 0 || len(remote.keys) != 0 {
				t.Fatalf("unstable name republished: %+v %v %v", result, err, remote.keys)
			}
			if got := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID); got.Name != after.Name {
				t.Fatal("unstable name cleared last good name")
			}
		})
	}
}

func TestExternalRenameKeepsAttemptedPendingBytesAcrossNewerName(t *testing.T) {
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript)
	reg := registration(t, path)
	reg.NativeSessionID = "01900000-0000-7000-8000-000000000001"
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore()}
	now := reg.RegisteredAt.Add(time.Hour)
	provider := &mutableLabels{}
	opts := Options{Sources: testSources, Parsers: testParsers, Labels: mutableLabelLookup{provider}, MachineID: "machine", Now: func() time.Time { return now }}
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) > 0 {
		t.Fatalf("%+v %v", result, err)
	}
	provider.label = archive.SessionLabel{State: archive.SessionLabelPresent, Name: "First rename", Source: archive.SessionLabelDatabase, Contract: codex.LabelContract}
	now = now.Add(time.Hour)
	remote.failMetadata = true
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) == 0 {
		t.Fatalf("expected durable failure: %+v %v", result, err)
	}
	pending, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found || !pending.Attempted {
		t.Fatalf("%+v %v %v", pending, found, err)
	}
	provider.label.Name = "Second rename"
	now = now.Add(time.Hour)
	remote.failMetadata = false
	result, err = Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) > 0 {
		t.Fatalf("%+v %v", result, err)
	}
	if got := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID); got.Name != "First rename" {
		t.Fatalf("pending bytes replaced: %q", got.Name)
	}
	now = now.Add(time.Hour)
	result, err = Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) > 0 {
		t.Fatalf("%+v %v", result, err)
	}
	if got := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID); got.Name != "Second rename" {
		t.Fatalf("newer fingerprint lost after retry: %q", got.Name)
	}
}

func TestExternalRenameCombinesChangedConversationWithCurrentName(t *testing.T) {
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
	filter := &operationFilter{}
	bindings := &operationBindings{parser: &operationParser{version: "0.1.0"}, filter: filter}
	opts := Options{Sources: bindings, Parsers: bindings, Labels: mutableLabelLookup{provider}, MachineID: "machine", Now: func() time.Time { return now }}
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) > 0 {
		t.Fatalf("%+v %v", result, err)
	}
	before := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("\n{\"type\":\"event_msg\",\"timestamp\":\"2026-09-22T12:00:00Z\",\"payload\":{\"type\":\"user_message\",\"message\":\"Fresh conversation content\"}}\n")
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	provider.label = archive.SessionLabel{State: archive.SessionLabelPresent, Name: "Current rename", Source: archive.SessionLabelDatabase, Contract: codex.LabelContract}
	filter.calls = 0
	now = now.Add(time.Hour)
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) > 0 {
		t.Fatalf("%+v %v", result, err)
	}
	after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if filter.calls != 1 || after.Name != "Current rename" || len(fetchBundle(t, remote, after).NativeRecords) <= len(fetchBundle(t, remote, before).NativeRecords) || !after.CapturedAt.Equal(now) {
		t.Fatalf("changed source replaced by old retained source: filtered=%d before=%+v after=%+v", filter.calls, before, after)
	}
}

func TestLabelCapabilityUsesRegistrationHarnessAndGenericContext(t *testing.T) {
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "session.jsonl", `{"type":"user","uuid":"message-1","message":{"role":"user","content":"Invented prompt"}}`)
	reg := registration(t, path)
	reg.Harness = archive.Harness{Name: "claude", Version: "2.1.0"}
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", Now: func() time.Time { return now }}
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) > 0 {
		t.Fatalf("%+v %v", result, err)
	}
	provider := &fairLabels{requested: map[string]bool{}}
	opts.Labels = provider
	now = now.Add(time.Hour)
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) > 0 {
		t.Fatalf("%+v %v", result, err)
	}
	if !provider.requested[reg.ArchiveSessionID] {
		t.Fatal("non-Codex injected label capability was not selected")
	}
	cache, err := local.LoadLabels()
	if err != nil {
		t.Fatal(err)
	}
	if entry, ok := cache.Entries[reg.ArchiveSessionID]; !ok || entry.Context.Producer != "2.1.0" {
		t.Fatalf("generic producer/context not durable: %+v", cache)
	}
}
