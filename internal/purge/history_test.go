package purge

import (
	"encoding/json"
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
		body := []byte("synthetic " + c)
		sha := storage.SHA256Hex(body)
		key := prefix + "source." + sha + ".jsonl.gz"
		keys = append(keys, key)
		if err := remote.Put(t.Context(), key, body); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, archive.SourceReference{Key: key, SHA256: sha, CompressedBytes: len(body)})
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
	m.History.Preserved = append(m.History.Preserved, archive.RevisionReference{RevisionID: "44444444-4444-4444-8444-444444444444", CapturedAt: time.Now(), Source: archive.SourceReference{Key: keys[3], SHA256: storage.SHA256Hex([]byte("synthetic d")), CompressedBytes: len("synthetic d")}})
	putHistoryMetadata(t, remote, key, m)
	report := Report{PlanDigest: plan.Digest, Remaining: []string{keys[3]}}
	if err = Apply(t.Context(), remote, plan, &report, func(Report) error { return nil }); err == nil {
		t.Fatal("new preserved reference deleted")
	}
}

type historyFailure string

const (
	historyMissing    historyFailure = "missing"
	historyInvalid    historyFailure = "invalid"
	historyUnreadable historyFailure = "unreadable"
)

func TestHistoryPurgeMissingOrIncompletePreservedMetadataFailsClosed(t *testing.T) {
	for _, kind := range []historyFailure{historyMissing, historyInvalid, historyUnreadable} {
		t.Run(string(kind), func(t *testing.T) {
			remote, m, key, keys := historyInventory(t)
			switch kind {
			case historyMissing:
				if err := remote.Delete(t.Context(), keys[1]); err != nil {
					t.Fatal(err)
				}
			case historyInvalid:
				m.History.Preserved[0].CapturedAt = time.Time{}
				putHistoryMetadata(t, remote, key, m)
			case historyUnreadable:
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

func TestPurgeCorruptSelectedSourcePreventsPlanAndApply(t *testing.T) {
	for _, beforePlan := range []bool{true, false} {
		t.Run(map[bool]string{true: "plan", false: "apply"}[beforePlan], func(t *testing.T) {
			remote, _, _, keys := historyInventory(t)
			var plan Plan
			var err error
			if !beforePlan {
				plan, err = Inventory(t.Context(), remote, "d", "b", "", ModeUnreferenced, "", time.Now())
				if err != nil {
					t.Fatal(err)
				}
			}
			if err = remote.Put(t.Context(), keys[1], []byte("damaged preserved object")); err != nil {
				t.Fatal(err)
			}
			if beforePlan {
				_, err = Inventory(t.Context(), remote, "d", "b", "", ModeUnreferenced, "", time.Now())
			} else {
				report := Report{PlanDigest: plan.Digest, Remaining: []string{keys[3]}}
				err = Apply(t.Context(), remote, plan, &report, func(Report) error { return nil })
			}
			if err == nil {
				t.Fatal("corrupt selected source authorized purge")
			}
			if _, err = remote.Get(t.Context(), keys[3]); err != nil {
				t.Fatal("candidate removed", err)
			}
		})
	}
}
