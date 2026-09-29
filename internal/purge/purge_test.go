package purge

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

const session = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

const sourceA = "sessions/claude/" + session + "/source." + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + ".jsonl.gz"

const sourceB = "sessions/claude/" + session + "/source." + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" + ".jsonl.gz"

const sidecar = "sessions/claude/" + session + "/metadata.json"

func putMetadata(t *testing.T, store storage.ObjectStore, source, filter string) {
	t.Helper()
	meta := archive.Metadata{SchemaVersion: archive.MetadataSchemaVersion, SourceBundle: archive.SourceReference{Key: source, SHA256: strings.Repeat("a", 64)}, FilterVersion: filter}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), sidecar, data); err != nil {
		t.Fatal(err)
	}
}

func compressedHeader(t *testing.T, version string) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	header := archive.SourceHeader{Kind: archive.SourceLineHeader, Capture: archive.SourceCapture{FilterVersion: version}}
	if err := json.NewEncoder(writer).Encode(header); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func fixture(t *testing.T) (*storagetest.MemoryStore, time.Time) {
	t.Helper()
	store := storagetest.NewMemoryStore()
	ctx := context.Background()
	if err := store.Put(ctx, sourceA, compressedHeader(t, "10")); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, sourceB, compressedHeader(t, "9")); err != nil {
		t.Fatal(err)
	}
	putMetadata(t, store, sourceA, "10")
	return store, time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
}

func TestInventorySeparatesCurrentOldSource(t *testing.T) {
	store, now := fixture(t)
	plan, err := Inventory(context.Background(), store, "destination", "bucket", "archive/", ModeOldFilter, "11", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].Key != sourceB || plan.Candidates[0].FilterVersion != "9" {
		t.Fatalf("candidates: %#v", plan.Candidates)
	}
	if len(plan.CurrentOldSessions) != 1 || plan.CurrentOldSessions[0].SourceKey != sourceA {
		t.Fatalf("current: %#v", plan.CurrentOldSessions)
	}
	if err := plan.Validate("destination", now); err != nil {
		t.Fatal(err)
	}
	if err := plan.Validate("other", now); err == nil {
		t.Fatal("changed destination accepted")
	}
	if err := plan.Validate("destination", now.Add(Lifetime)); err == nil {
		t.Fatal("expired plan accepted")
	}
	plan.Candidates[0].Key = sourceA
	if err := plan.Validate("destination", now); err == nil {
		t.Fatal("tampered plan accepted")
	}
}

func TestApplyRefusesNewReferenceAndUnreadableMetadata(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		store, now := fixture(t)
		plan, err := Inventory(context.Background(), store, "destination", "bucket", "", ModeUnreferenced, "", now)
		if err != nil {
			t.Fatal(err)
		}
		if invalid {
			if err := store.Put(context.Background(), sidecar, []byte("bad")); err != nil {
				t.Fatal(err)
			}
		} else {
			putMetadata(t, store, sourceB, "9")
		}
		report := Report{PlanDigest: plan.Digest, Remaining: []string{sourceB}}
		err = Apply(context.Background(), store, plan, &report, func(Report) error { return nil })
		if err == nil {
			t.Fatal("deleted source despite changed or unreadable metadata")
		}
		if _, err := store.Get(context.Background(), sourceB); err != nil {
			t.Fatalf("source removed: %v", err)
		}
	}
}

type failDelete struct {
	storage.ObjectStore
	once bool
}

func (f *failDelete) Delete(ctx context.Context, key string) error {
	if !f.once {
		f.once = true
		return errors.New("denied")
	}
	return f.ObjectStore.Delete(ctx, key)
}

func TestApplyRetriesPartialFailureAndChecksIdentity(t *testing.T) {
	store, now := fixture(t)
	plan, err := Inventory(context.Background(), store, "destination", "bucket", "", ModeUnreferenced, "", now)
	if err != nil {
		t.Fatal(err)
	}
	report := Report{PlanDigest: plan.Digest, Remaining: []string{sourceB}}
	failing := &failDelete{ObjectStore: store}
	var saves int
	save := func(Report) error { saves++; return nil }
	if err := Apply(context.Background(), failing, plan, &report, save); err == nil {
		t.Fatal("delete failure suppressed")
	}
	if len(report.Remaining) != 1 || saves != 0 {
		t.Fatalf("report after failure: %#v, saves %d", report, saves)
	}
	if err := Apply(context.Background(), failing, plan, &report, save); err != nil {
		t.Fatal(err)
	}
	if len(report.Remaining) != 0 || len(report.Deleted) != 1 || saves != 1 {
		t.Fatalf("report after retry: %#v, saves %d", report, saves)
	}
	if _, err := store.Get(context.Background(), sourceA); err != nil {
		t.Fatalf("current source removed: %v", err)
	}
}

func TestInventoryStopsOnUnreadableSidecar(t *testing.T) {
	store, now := fixture(t)
	if err := store.Put(context.Background(), sidecar, []byte("bad")); err != nil {
		t.Fatal(err)
	}
	if _, err := Inventory(context.Background(), store, "destination", "bucket", "", ModeUnreferenced, "", now); err == nil {
		t.Fatal("planned from unreadable metadata")
	}
}

func TestInventoryStopsWhenCurrentSourceIsMissing(t *testing.T) {
	store, now := fixture(t)
	if err := store.Delete(context.Background(), sourceA); err != nil {
		t.Fatal(err)
	}
	if _, err := Inventory(context.Background(), store, "destination", "bucket", "", ModeUnreferenced, "", now); err == nil {
		t.Fatal("planned while current source was missing")
	}
}

func TestApplyRefusesChangedCandidate(t *testing.T) {
	store, now := fixture(t)
	plan, err := Inventory(context.Background(), store, "destination", "bucket", "", ModeUnreferenced, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), sourceB, []byte("replaced")); err != nil {
		t.Fatal(err)
	}
	report := Report{PlanDigest: plan.Digest, Remaining: []string{sourceB}}
	if err := Apply(context.Background(), store, plan, &report, func(Report) error { return nil }); err == nil {
		t.Fatal("deleted changed source")
	}
}

func TestApplyFailsClosedOnNewAmbiguousMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		put  func(*testing.T, storage.ObjectStore)
	}{
		{
			name: "unexpected sidecar path",
			put: func(t *testing.T, store storage.ObjectStore) {
				t.Helper()
				if err := store.Put(context.Background(), "sessions/claude/other/metadata.json", []byte("{}")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "missing current source",
			put: func(t *testing.T, store storage.ObjectStore) {
				t.Helper()
				if err := store.Delete(context.Background(), sourceA); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "invalid source reference",
			put: func(t *testing.T, store storage.ObjectStore) {
				t.Helper()
				putMetadata(t, store, "sessions/claude/other/source."+strings.Repeat("a", 64)+".jsonl.gz", "10")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, now := fixture(t)
			plan, err := Inventory(context.Background(), store, "destination", "bucket", "", ModeUnreferenced, "", now)
			if err != nil {
				t.Fatal(err)
			}
			tc.put(t, store)
			report := Report{PlanDigest: plan.Digest, Remaining: []string{sourceB}}
			if err := Apply(context.Background(), store, plan, &report, func(Report) error { return nil }); err == nil {
				t.Fatal("applied plan despite ambiguous current metadata")
			}
			if _, err := store.Get(context.Background(), sourceB); err != nil {
				t.Fatalf("candidate removed: %v", err)
			}
		})
	}
}
