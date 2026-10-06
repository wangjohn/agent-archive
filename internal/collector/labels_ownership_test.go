package collector

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestExternalRenameRequiresCurrentPublishedOwnerBeforeLookup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		warm   bool
		mutate string
	}{
		{"current machine changed", false, "machine"},
		{"current machine changed with cached context", true, "machine"},
		{"published owner changed with same source", true, "owner"},
		{"owed cached name during owner-change backoff", true, "owner-backoff"},
		{"published archive identity changed", true, "archive"},
		{"published native identity changed", true, "native"},
		{"published metadata missing", true, "missing"},
		{"published metadata malformed", true, "malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := newTestStore(t)
			path := writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript)
			reg := registration(t, path)
			reg.NativeSessionID = "01900000-0000-7000-8000-000000000001"
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			remote := &countedPublications{MemoryStore: storagetest.NewMemoryStore()}
			now := reg.RegisteredAt.Add(time.Hour)
			provider := &mutableLabels{}
			filter := &operationFilter{}
			bindings := &operationBindings{parser: &operationParser{version: "0.1.0"}, filter: filter}
			opts := Options{Sources: bindings, Parsers: bindings, Labels: mutableLabelLookup{provider}, MachineID: "old-owner", Now: func() time.Time { return now }}
			run := func() {
				t.Helper()
				if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) > 0 {
					t.Fatalf("%+v %v", result, err)
				}
			}
			run()
			if tc.warm {
				now = now.Add(time.Hour)
				run()
			}
			before := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
			if tc.mutate == "owner-backoff" {
				cache, err := local.LoadLabels()
				if err != nil {
					t.Fatal(err)
				}
				entry := cache.Entries[reg.ArchiveSessionID]
				entry.Label = archive.SessionLabel{NativeID: reg.NativeSessionID, State: archive.SessionLabelPresent, Name: "Unpublished cached name", Source: archive.SessionLabelDatabase, Contract: codex.LabelContract}
				entry.ObservedAt, entry.NextAt = now, now.Add(time.Hour)
				cache.Entries[reg.ArchiveSessionID] = entry
				if err := local.SaveLabels(cache); err != nil {
					t.Fatal(err)
				}
			}
			originalChecksum, _, err := local.LabelRevision(reg.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.mutate == "machine" {
				opts.MachineID = "new-owner"
			} else {
				published, err := local.LoadPublishedState(reg.ArchiveSessionID)
				if err != nil {
					t.Fatal(err)
				}
				metadata := before
				switch tc.mutate {
				case "owner", "owner-backoff":
					metadata.MachineID = "other-owner"
				case "archive":
					metadata.SessionID = "other-session"
				case "native":
					metadata.NativeSessionID = "other-native"
				}
				encoded, err := json.Marshal(metadata)
				if err != nil {
					t.Fatal(err)
				}
				if tc.mutate == "missing" {
					encoded = nil
				}
				if tc.mutate == "malformed" {
					encoded = []byte("malformed")
				}
				if err := published.CacheMetadata(encoded); err != nil {
					t.Fatal(err)
				}
				checksum, _, err := local.LabelRevision(reg.ArchiveSessionID)
				if err != nil || checksum != originalChecksum {
					t.Fatalf("owner edit changed source revision: %s %s %v", originalChecksum, checksum, err)
				}
			}
			if tc.mutate == "owner-backoff" {
				now = now.Add(30 * time.Second)
			} else {
				now = now.Add(time.Hour)
			}
			provider.label = archive.SessionLabel{State: archive.SessionLabelPresent, Name: "New native name", Source: archive.SessionLabelDatabase, Contract: codex.LabelContract}
			calls := provider.calls
			filter.calls = 0
			remote.keys = nil
			run()
			after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
			if provider.calls != calls || filter.calls != 0 || len(remote.keys) != 0 || after.MachineID != before.MachineID || after.Name != before.Name || after.SourceBundle != before.SourceBundle {
				t.Fatalf("foreign label caused native or publication work: calls=%d/%d filters=%d before=%+v after=%+v keys=%v", calls, provider.calls, filter.calls, before, after, remote.keys)
			}
		})
	}
}
