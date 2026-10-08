package reader

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type syntheticCatalogStore struct{ *storagetest.MemoryStore }

func (*syntheticCatalogStore) CatalogAtomicQualification() error { return nil }

func remoteReaderFixture(t *testing.T, count int) (*catalog.Store, *storagetest.MemoryStore) {
	t.Helper()
	raw := &syntheticCatalogStore{storagetest.NewMemoryStore()}
	legacy := storagetest.NewMemoryStore()
	w, err := catalog.New(raw)
	if err != nil {
		t.Fatal(err)
	}
	for i := range count {
		id := fmt.Sprintf("session-%04d", i)
		source := []byte("synthetic source " + id)
		sha := storage.SHA256Hex(source)
		sourceKey := "sessions/claude/" + id + "/source." + sha + ".jsonl.gz"
		m := archive.Metadata{SchemaVersion: 1, SessionID: id, Harness: archive.Harness{Name: "claude"}, ProjectID: "project", Title: "Résumé #212", CapturedAt: baseTime.Add(time.Duration(i) * time.Minute), SourceBundle: archive.SourceReference{Key: sourceKey, SHA256: sha, CompressedBytes: len(source)}}
		if i%4 == 0 {
			m.ParentSessionID = "session-0001"
		}
		if i%7 == 0 {
			m.Replay = &archive.Replay{}
		}
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		key, err := archive.MetadataObjectKey("claude", id)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []*storagetest.MemoryStore{raw.MemoryStore, legacy} {
			if err = s.Put(t.Context(), sourceKey, source); err != nil {
				t.Fatal(err)
			}
		}
		if err = legacy.Put(t.Context(), key, data); err != nil {
			t.Fatal(err)
		}
		ref, err := w.PutImmutable(t.Context(), catalog.KindMetadata, data)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Commit(t.Context(), catalog.CatalogMutation{ID: "fixture/" + id, SessionKey: key, Next: &catalog.CatalogEntry{Metadata: ref, Summary: m}}); err != nil {
			t.Fatal(err)
		}
	}
	c := w.Coordinator()
	owner, err := c.Seal(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Activate(t.Context(), owner, "private synthetic cutover"); err != nil {
		t.Fatal(err)
	}
	if err = c.Release(t.Context(), owner); err != nil {
		t.Fatal(err)
	}
	remote, err := catalog.Wrap(raw)
	if err != nil {
		t.Fatal(err)
	}
	return remote, legacy
}

func TestRemoteRootSelectionMatchesOracleWithoutCanonicalList(t *testing.T) {
	remote, legacy := remoteReaderFixture(t, 160)
	measured := storagetest.NewMeasuredStore(remote, 0)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range []QueryOrder{CaptureOrder, ActivityOrder} {
		q := MetadataQuery{Filter: Filter{Replays: ReplaysHidden}, Limit: 10, Order: order, TopLevelOnly: true}
		result, err := SelectMetadata(t.Context(), measured, "sessions", q, ListOptions{Cache: cache})
		if err != nil {
			t.Fatal(err)
		}
		expected, err := SelectMetadata(t.Context(), legacy, "sessions", q, ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(result.Sessions, expected.Sessions) || result.Hidden != expected.Hidden || result.TotalMatched != expected.TotalMatched {
			t.Fatalf("oracle mismatch got %+v expected %+v", result, expected)
		}
		for _, row := range result.Sessions {
			key := row.Harness.Name + "/" + row.SessionID
			if result.Children[key] != expected.Children[key] {
				t.Fatal("child count differs")
			}
		}
		if metrics := measured.Metrics(); metrics.Lists != 0 {
			t.Fatalf("canonical LIST %+v", metrics)
		}
	}
	measured.Reset()
	if _, err = SelectMetadata(t.Context(), measured, "sessions", MetadataQuery{Filter: Filter{From: baseTime.Add(1000 * time.Hour)}, Limit: 10}, ListOptions{Cache: cache}); err != nil {
		t.Fatal(err)
	}
	if metrics := measured.Metrics(); metrics.Lists != 0 || metrics.Gets > 10 {
		t.Fatalf("future bounded query %+v", metrics)
	}
}

func TestRemoteSummaryDeltaDeletesAndUnknownRootRebuilds(t *testing.T) {
	remote, _ := remoteReaderFixture(t, 40)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := OpenSessionCatalog(t.Context(), cache, remote, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.RefreshRemote(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, err := c.Query(t.Context(), CatalogQuery{})
	if err != nil || before.Total != 40 {
		t.Fatal(before.Total, err)
	}
	key := "sessions/claude/session-0001/metadata.json"
	if err = remote.DeleteSession(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	if err = c.RefreshRemote(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := c.Query(t.Context(), CatalogQuery{})
	if err != nil || after.Total != 39 {
		t.Fatal(after.Total, err)
	}
	for _, row := range after.Rows {
		if row.Key == key {
			t.Fatal("deleted summary resurrected")
		}
	}
	if _, err = c.db.ExecContext(t.Context(), "UPDATE remote_root SET root=?", []byte(`{"Key":"catalog-v4/nodes/missing.json","SHA256":"missing"}`)); err != nil {
		t.Fatal(err)
	}
	if err = c.RefreshRemote(t.Context()); err != nil {
		t.Fatal("unknown root rebuild", err)
	}
	after, err = c.Query(t.Context(), CatalogQuery{})
	if err != nil || after.Total != 39 {
		t.Fatal(after.Total, err)
	}
}
