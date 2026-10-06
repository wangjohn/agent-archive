package collector

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
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
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "rotated"}[missing], func(t *testing.T) {
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
			if missing {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			now = now.Add(time.Hour)
			provider.label = archive.SessionLabel{State: "present", Name: "Invented native rename", Source: "database", Contract: archive.SessionLabelContract}
			filter.calls = 0
			remote.keys = nil
			result, err = Run(context.Background(), local, remote, opts)
			if err != nil || len(result.Errors) > 0 || len(result.Published) != 1 {
				t.Fatalf("%+v %v", result, err)
			}
			after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
			afterBundle := fetchBundle(t, remote, after)
			if filter.calls != 0 {
				t.Fatalf("name-only refresh filtered native input %d times", filter.calls)
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
	provider.label = archive.SessionLabel{State: "present", Name: "First rename", Source: "database", Contract: archive.SessionLabelContract}
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
