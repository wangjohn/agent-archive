package retention

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func retainedCleanup(t *testing.T) (*sweeper, archive.SessionRegistration, archive.Metadata, []string) {
	t.Helper()
	remote := storagetest.NewMemoryStore()
	local := newTestStore(t)
	reg := registration("synthetic", "")
	reg.NativeSessionID = "11111111-1111-4111-8111-111111111111"
	now := time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC)
	refs := []archive.SourceReference{}
	keys := []string{}
	for _, value := range []string{"current", "first preserved", "second preserved", "obsolete first", "obsolete last"} {
		raw := []byte(value)
		sha := storage.SHA256Hex(raw)
		key := "sessions/codex/synthetic/source." + sha + ".jsonl.gz"
		if err := remote.Put(t.Context(), key, raw); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
		refs = append(refs, archive.SourceReference{Key: key, SHA256: sha, CompressedBytes: len(raw)})
	}
	m := archive.Metadata{SchemaVersion: 2, SessionID: reg.ArchiveSessionID, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, Harness: reg.Harness, CapturedAt: now, SourceBundle: refs[0], History: &archive.RevisionHistory{CurrentRevision: reg.NativeSessionID, Preserved: []archive.RevisionReference{{RevisionID: "22222222-2222-4222-8222-222222222222", CapturedAt: now.Add(-time.Hour), Source: refs[1]}, {RevisionID: "33333333-3333-4333-8333-333333333333", CapturedAt: now.Add(-2 * time.Hour), Source: refs[2]}}}}
	raw, _ := json.Marshal(m)
	if err := remote.Put(t.Context(), "sessions/codex/synthetic/metadata.json", raw); err != nil {
		t.Fatal(err)
	}
	s := &sweeper{ctx: t.Context(), local: local, store: remote, opts: agreeing(Options{Now: func() time.Time { return now }, PrivacyVerified: func(archive.SessionRegistration, archive.Metadata) bool { return true }}), now: now, result: Result{Errors: map[string]error{}}, metadataSHA: storage.SHA256Hex(raw)}
	return s, reg, m, keys
}

func cleanupLedger(keys []string, now time.Time) []state.SupersededSource {
	entries := []state.SupersededSource{}
	for _, key := range keys {
		entries = append(entries, state.SupersededSource{Key: key, SupersededAt: now.Add(-48 * time.Hour), PrivacySensitive: true})
	}
	return entries
}

func TestRetainedCleanupProtectsFullUnionAndRetiresAllObsoletePrivacyKeys(t *testing.T) {
	s, reg, m, keys := retainedCleanup(t)
	if err := s.deleteSuperseded(reg, cleanupLedger(keys, s.now), m); err != nil {
		t.Fatal(err)
	}
	if s.result.DeletedSnapshots != 2 {
		t.Fatal(s.result)
	}
	for _, key := range keys[:3] {
		if _, err := s.store.Get(t.Context(), key); err != nil {
			t.Fatal("selected retained source removed", err)
		}
	}
	for _, key := range keys[3:] {
		if _, err := s.store.Get(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
			t.Fatal("obsolete privacy source remains", err)
		}
	}
}

func TestRetainedCleanupMissingPreservedSourceHoldsEveryObsoleteKey(t *testing.T) {
	s, reg, m, keys := retainedCleanup(t)
	if err := s.store.Delete(t.Context(), keys[1]); err != nil {
		t.Fatal(err)
	}
	if err := s.deleteSuperseded(reg, cleanupLedger(keys[3:], s.now), m); err == nil {
		t.Fatal("missing preserved reference authorized cleanup")
	}
	for _, key := range keys[3:] {
		if _, err := s.store.Get(t.Context(), key); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRetainedCleanupSelectingMetadataChangeHoldsPrivacyKeys(t *testing.T) {
	s, reg, m, keys := retainedCleanup(t)
	m.Title = "new selecting winner"
	raw, _ := json.Marshal(m)
	if err := s.store.Put(t.Context(), "sessions/codex/synthetic/metadata.json", raw); err != nil {
		t.Fatal(err)
	}
	if err := s.deleteSuperseded(reg, cleanupLedger(keys[3:], s.now), m); err == nil {
		t.Fatal("changed selecting metadata accepted")
	}
	for _, key := range keys[3:] {
		if _, err := s.store.Get(t.Context(), key); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRetainedCleanupRefusesWinnerChangedDuringSourceVerification(t *testing.T) {
	s, reg, m, keys := retainedCleanup(t)
	original := m
	m.History = &archive.RevisionHistory{CurrentRevision: m.History.CurrentRevision, Preserved: append([]archive.RevisionReference(nil), m.History.Preserved...)}
	m.History.Preserved = append(m.History.Preserved, archive.RevisionReference{RevisionID: "44444444-4444-4444-8444-444444444444", CapturedAt: s.now, Source: archive.SourceReference{Key: keys[3], SHA256: storage.SHA256Hex([]byte("obsolete first")), CompressedBytes: len("obsolete first")}})
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	s.store = &winnerDuringVerification{ObjectStore: s.store, key: "sessions/codex/synthetic/metadata.json", metadata: raw}
	if err := s.deleteSuperseded(reg, cleanupLedger(keys[3:], s.now), original); err == nil {
		t.Fatal("changed winner authorized superseded deletion")
	}
	if _, err := s.store.Get(t.Context(), keys[3]); err != nil {
		t.Fatal("new selected source deleted", err)
	}
}
