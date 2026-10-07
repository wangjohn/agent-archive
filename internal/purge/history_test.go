package purge

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func historyInventory(t *testing.T) (*storagetest.MemoryStore, archive.Metadata, string, []string) {
	t.Helper()
	remote := storagetest.NewMemoryStore()
	prefix := "sessions/codex/" + session + "/"
	keys := []string{}
	refs := []archive.SourceReference{}
	for _, c := range []string{"a", "b", "c", "d"} {
		key := prefix + "source." + strings.Repeat(c, 64) + ".jsonl.gz"
		keys = append(keys, key)
		if err := remote.Put(t.Context(), key, []byte("synthetic")); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, archive.SourceReference{Key: key, SHA256: strings.Repeat(c, 64), CompressedBytes: 9})
	}
	m := archive.Metadata{SchemaVersion: 2, ProjectID: "synthetic-project", SessionID: session, NativeSessionID: "11111111-1111-4111-8111-111111111111", Harness: archive.Harness{Name: "codex"}, SourceBundle: refs[0], FilterVersion: "10", CapturedAt: time.Now(), History: &archive.RevisionHistory{CurrentRevision: "11111111-1111-4111-8111-111111111111", Preserved: []archive.RevisionReference{{RevisionID: "22222222-2222-4222-8222-222222222222", CapturedAt: time.Now(), Source: refs[1]}, {RevisionID: "33333333-3333-4333-8333-333333333333", CapturedAt: time.Now(), Source: refs[2]}}}}
	key := prefix + "metadata.json"
	putHistoryMetadata(t, remote, key, m)
	return remote, m, key, keys
}
func putHistoryMetadata(t *testing.T, remote storage.ObjectStore, key string, m archive.Metadata) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err = remote.Put(t.Context(), key, b); err != nil {
		t.Fatal(err)
	}
}
func TestHistoryPurgeProtectsCurrentAndEveryPreservedRevision(t *testing.T) {
	remote, _, _, keys := historyInventory(t)
	plan, err := Inventory(t.Context(), remote, "destination", "bucket", "", ModeUnreferenced, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].Key != keys[3] {
		t.Fatal(plan.Candidates)
	}
	report := Report{PlanDigest: plan.Digest, Remaining: []string{keys[3]}}
	if err = Apply(t.Context(), remote, plan, &report, func(Report) error { return nil }); err != nil {
		t.Fatal(err)
	}
	for _, key := range keys[:3] {
		if _, err = remote.Get(t.Context(), key); err != nil {
			t.Fatal("referenced source deleted", err)
		}
	}
}
func TestHistoryPurgeRechecksNewPreservedReference(t *testing.T) {
	remote, m, key, keys := historyInventory(t)
	plan, err := Inventory(t.Context(), remote, "d", "b", "", ModeUnreferenced, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	m.History.Preserved = append(m.History.Preserved, archive.RevisionReference{RevisionID: "44444444-4444-4444-8444-444444444444", CapturedAt: time.Now(), Source: archive.SourceReference{Key: keys[3], SHA256: strings.Repeat("d", 64), CompressedBytes: 9}})
	putHistoryMetadata(t, remote, key, m)
	report := Report{PlanDigest: plan.Digest, Remaining: []string{keys[3]}}
	if err = Apply(t.Context(), remote, plan, &report, func(Report) error { return nil }); err == nil {
		t.Fatal("new preserved reference deleted")
	}
}
func TestHistoryPurgeMissingOrIncompletePreservedMetadataFailsClosed(t *testing.T) {
	for _, kind := range []string{"missing", "invalid", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			remote, m, key, keys := historyInventory(t)
			switch kind {
			case "missing":
				if err := remote.Delete(t.Context(), keys[1]); err != nil {
					t.Fatal(err)
				}
			case "invalid":
				m.History.Preserved[0].CapturedAt = time.Time{}
				putHistoryMetadata(t, remote, key, m)
			case "unreadable":
				if err := remote.Put(t.Context(), key, []byte("{")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Inventory(t.Context(), remote, "d", "b", "", ModeUnreferenced, "", time.Now()); err == nil {
				t.Fatal("incomplete selecting metadata accepted")
			}
			if _, err := remote.Get(t.Context(), keys[0]); err != nil {
				t.Fatal(err)
			}
		})
	}
}
