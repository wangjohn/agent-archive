package listingindex_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"github.com/wangjohn/agent-archive/internal/testutil/providertest"
)

type componentBoundStore struct{ *storagetest.MemoryStore }

func (s componentBoundStore) Put(ctx context.Context, key string, body []byte) error {
	for part := range strings.SplitSeq(key, "/") {
		if len(part) > 255 {
			return errors.New("synthetic filesystem component limit")
		}
	}
	return s.MemoryStore.Put(ctx, key, body)
}

func TestPortableRevisionPreservesExactSummaryAndBoundsEveryWrite(t *testing.T) {
	store := componentBoundStore{storagetest.NewMemoryStore()}
	f := providertest.PutRetainedFixture(t, store, 64, time.Now().UTC())
	key := "sessions/codex/" + f.Registration.ArchiveSessionID + "/metadata.json"
	r, err := listingindex.NewRevision(key, f.Body, strings.Repeat("e", 32))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Key, "/c/") {
		t.Fatal("long summary did not use portable chunks")
	}
	parsed, err := listingindex.ParseRevision(r.Key)
	if err != nil || !r.SameSummary(parsed) || r.Nonce != parsed.Nonce || r.ETag != parsed.ETag {
		t.Fatal("portable summary changed identity", err)
	}
	if err := listingindex.PublishRevision(t.Context(), store, key, f.Body); err != nil {
		t.Fatal("repair regenerated unsafe key", err)
	}
	hints, err := store.List(t.Context(), listingindex.V3Prefix)
	if err != nil || len(hints) != 1 {
		t.Fatal(hints, err)
	}
	for part := range strings.SplitSeq(hints[0].Key, "/") {
		if len(part) > 240 {
			t.Fatal("portable component exceeds bound", len(part))
		}
	}
	if _, err := listingindex.NewRevision(key, f.Body, strings.Repeat("e", 800)); err == nil {
		t.Fatal("overbudget summary accepted")
	}
}

func TestPortableRevisionRejectsNoncanonicalChunks(t *testing.T) {
	store := storagetest.NewMemoryStore()
	f := providertest.PutRetainedFixture(t, store, 1, time.Now().UTC())
	key := "sessions/codex/" + f.Registration.ArchiveSessionID + "/metadata.json"
	r, err := listingindex.NewRevision(key, f.Body, strings.Repeat("e", 32))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(r.Key, "/")
	if len(parts) < 7 || parts[5] != "c" {
		t.Fatal("fixture did not use chunks")
	}
	shifted := append([]string(nil), parts...)
	shifted[6] += shifted[7][:1]
	shifted[7] = shifted[7][1:]
	shortened := append([]string(nil), parts...)
	shortened[7] = shortened[6][239:] + shortened[7]
	shortened[6] = shortened[6][:239]
	invalid := []string{strings.Join(shifted, "/"), strings.Join(shortened, "/"), r.Key + "/", strings.Repeat("x", 1025)}
	for _, key := range invalid {
		if _, err := listingindex.ParseRevision(key); err == nil {
			t.Fatal("noncanonical layout accepted")
		}
	}
	r.Nonce = ""
	r.ProjectID = "p"
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	unnecessary := strings.Join(parts[:6], "/") + "/" + base64.RawURLEncoding.EncodeToString(encoded)
	if _, err := listingindex.ParseRevision(unnecessary); err == nil {
		t.Fatal("unnecessary chunk layout accepted")
	}
}

func TestPortableRevisionMigratesAndDeletesMixedHistoricalHints(t *testing.T) {
	store := storagetest.NewMemoryStore()
	f := providertest.PutRetainedFixture(t, store, 2, time.Now().UTC())
	key := "sessions/codex/" + f.Registration.ArchiveSessionID + "/metadata.json"
	body, validator, err := store.GetVersioned(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	r, err := listingindex.NewRevision(key, body, validator)
	if err != nil {
		t.Fatal(err)
	}
	oldV2, oldV3 := historicalRevisionKeys(t, r, body)
	for _, key := range []string{oldV2, oldV3} {
		if _, err := listingindex.ParseRevision(key); err != nil {
			t.Fatal("historical single-component hint unreadable", err)
		}
		if err := store.Put(t.Context(), key, nil); err != nil {
			t.Fatal(err)
		}
	}
	oldSnapshot, err := listingindex.LegacyEntries(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	if err := listingindex.RebuildRevision(t.Context(), store, r, oldSnapshot[key]); err != nil {
		t.Fatal(err)
	}
	legacy, err := store.List(t.Context(), listingindex.V2Prefix)
	if err != nil || len(legacy) != 0 {
		t.Fatal("legacy cleanup incomplete", legacy, err)
	}
	hints, err := store.List(t.Context(), listingindex.V3Prefix)
	if err != nil || len(hints) != 1 || !strings.Contains(hints[0].Key, "/c/") {
		t.Fatal("portable survivor missing", hints, err)
	}
	if err := listingindex.DeleteSession(t.Context(), store, "codex", f.Registration.ArchiveSessionID); err != nil {
		t.Fatal(err)
	}
	hints, err = store.List(t.Context(), listingindex.V3Prefix)
	if err != nil || len(hints) != 0 {
		t.Fatal("chunked cleanup incomplete", hints, err)
	}
	actual, err := store.Get(t.Context(), key)
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatal("hint cleanup changed canonical metadata", err)
	}
}

func historicalRevisionKeys(t *testing.T, r listingindex.Revision, body []byte) (string, string) {
	t.Helper()
	legacy, err := listingindex.New(r.MetadataKey, body)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimPrefix(legacy.Key, listingindex.Prefix), "/")
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString(encoded)
	return listingindex.V2Prefix + strings.Join(parts[:3], "/") + "/" + payload, listingindex.V3Prefix + parts[1] + "/" + parts[2] + "/" + parts[0] + "/" + payload
}

func TestPortableRevisionBudgetIncludesChunkSeparators(t *testing.T) {
	store := storagetest.NewMemoryStore()
	f := providertest.PutRetainedFixture(t, store, 1, time.Now().UTC())
	key := "sessions/codex/" + f.Registration.ArchiveSessionID + "/metadata.json"
	var last listingindex.Revision
	refused := false
	for size := 1; size <= 1024; size++ {
		r, err := listingindex.NewRevision(key, f.Body, strings.Repeat("e", size))
		if err != nil {
			if last.Key == "" || len(last.Key) < 1020 {
				t.Fatal("budget refused small valid summary", len(last.Key), err)
			}
			last.ETag = strings.Repeat("e", size)
			_, old := historicalRevisionKeys(t, last, f.Body)
			if len(old) > 1024 {
				t.Fatal("fixture did not isolate chunk separator overhead", len(old))
			}
			refused = true
			break
		}
		if len(r.Key) > 1024 {
			t.Fatal("portable key exceeded total object budget")
		}
		last = r
	}
	if !refused {
		t.Fatal("unbounded encoded summary accepted")
	}
}
