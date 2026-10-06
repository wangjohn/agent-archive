package collector

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestPublicationConflictingWinnerRetainsJournalAndRepairsAuthoritativeListing(t *testing.T) {
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "synthetic.jsonl", codexTranscript)
	reg := registration(t, path)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", at); err != nil {
		t.Fatal(err)
	}
	remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic-machine", Now: func() time.Time { return at }}
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] == nil {
		t.Fatal(result, err)
	}
	pending, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found || pending.Commit == nil {
		t.Fatal(found, err)
	}
	var winner map[string]any
	if err := json.Unmarshal(pending.MetadataBytes, &winner); err != nil {
		t.Fatal(err)
	}
	winner["metadata_derived_at"] = at.Add(time.Hour).Format(time.RFC3339)
	body, err := json.Marshal(winner)
	if err != nil {
		t.Fatal(err)
	}
	remote.failMetadata = false
	if err := remote.MemoryStore.Put(t.Context(), pending.MetadataKey, body); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	result, err = Run(t.Context(), local, remote, opts)
	if err != nil || !errors.Is(result.Errors[reg.ArchiveSessionID], storage.ErrPublicationConflict) || len(result.Published) != 0 {
		t.Fatal(result, err)
	}
	got, err := remote.Get(t.Context(), pending.MetadataKey)
	if err != nil || string(got) != string(body) {
		t.Fatal("stale pending replaced winner", err)
	}
	if _, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || !found {
		t.Fatal("uncommitted request acknowledged", found, err)
	}
	repairs, err := local.ListingRepairs(32)
	if err != nil {
		t.Fatal(err)
	}
	// The conflict leaves a fresh repair journal; the previous pass's repair used
	// the authoritative winner. Check its immutable listing identity directly.
	hints, err := remote.List(t.Context(), listingindex.V3Prefix)
	if err != nil {
		t.Fatal(err)
	}
	winnerIndexed := false
	for _, hint := range hints {
		revision, err := listingindex.ParseRevision(hint.Key)
		if err != nil {
			t.Fatal(err)
		}
		if revision.Hash == storage.SHA256Hex(body) {
			winnerIndexed = true
		}
		if revision.Hash == pending.Commit.MetadataSHA256 {
			t.Fatal("obsolete pending body indexed")
		}
	}
	if !winnerIndexed {
		t.Fatal("authoritative listing wasn't repaired", repairs)
	}

}

type localCommitFaultStore struct {
	*storagetest.MemoryStore
	publishedPath string
	fail          bool
}

func (s *localCommitFaultStore) Put(ctx context.Context, key string, body []byte) error {
	if err := s.MemoryStore.Put(ctx, key, body); err != nil {
		return err
	}
	if s.fail && filepath.Base(key) == "metadata.json" {
		if err := os.Mkdir(s.publishedPath, 0700); err != nil {
			return err
		}
		s.fail = false
	}
	return nil
}

func TestPublicationLocalCommitFailureReplaysExactBytesAfterNativeDeletion(t *testing.T) {
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "synthetic.jsonl", codexTranscript)
	reg := registration(t, path)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", at); err != nil {
		t.Fatal(err)
	}
	remote := &localCommitFaultStore{MemoryStore: storagetest.NewMemoryStore(), publishedPath: publishedPath(local, reg.ArchiveSessionID), fail: true}
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic-machine", Now: func() time.Time { return at }}
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] == nil || len(result.Published) != 0 {
		t.Fatal(result, err)
	}
	pending, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if _, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || !found {
		t.Fatal("request completed before local commit", found, err)
	}
	if err := os.Remove(remote.publishedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	result, err = Run(t.Context(), reopened, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result, err)
	}
	published, err := reopened.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := published.CommittedSources()
	if err != nil || len(refs) != 1 || refs[0] != pending.SourceReference() || string(published.Metadata()) != string(pending.MetadataBytes) {
		t.Fatal("full local commit lost frozen bytes", refs, err)
	}
	if _, found, err := reopened.LoadRequest(reg.ArchiveSessionID); err != nil || found {
		t.Fatal("covered request not acknowledged", found, err)
	}
	if found, err := reopened.HasPending(reg.ArchiveSessionID); err != nil || found {
		t.Fatal("committed journal not released", found, err)
	}
}

func TestPublicationLegacyRemoteCacheKeepsUnknownPredecessor(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "synthetic.jsonl", codexTranscript))
	remote := storagetest.NewMemoryStore()
	at := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic-machine", Now: func() time.Time { return at }}
	publishOnce(t, local, remote, reg, &opts)
	key := "sessions/codex/session-1/metadata.json"
	old, err := remote.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	editPublishedState(t, local, func(raw map[string]any) { delete(raw, "metadata_bytes") })
	stopAtNewCommit(t, local, reg, at.Add(time.Minute))
	at = at.Add(time.Hour)
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || !errors.Is(result.Errors[reg.ArchiveSessionID], storage.ErrPublicationConflict) {
		t.Fatal(result, err)
	}
	got, err := remote.Get(t.Context(), key)
	if err != nil || string(got) != string(old) {
		t.Fatal("legacy remote body guessed as predecessor", err)
	}
	reopened, err := state.Open(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	published, err := reopened.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil || published.PublicationPredecessor().State != state.PredecessorUnknown {
		t.Fatal("legacy authority changed after restart", err)
	}
}

func TestPublicationListingRepairRefusesCorruptAuthoritativeSource(t *testing.T) {
	local := newTestStore(t)
	claudeSession(t, local, claudePromptLine+"\n")
	remote := &failingListingStore{MemoryStore: storagetest.NewMemoryStore(), fail: true, fault: errors.New("synthetic listing interruption")}
	at := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	result := runAt(t, local, remote, at)
	if len(result.Published) != 1 {
		t.Fatal(result)
	}
	m := fetchMetadata(t, remote, "claude", "session-1")
	if err := remote.MemoryStore.Put(t.Context(), m.SourceBundle.Key, []byte("corrupt referenced source")); err != nil {
		t.Fatal(err)
	}
	remote.fail = false
	result = runAt(t, local, remote, at.Add(time.Minute))
	if !errors.Is(result.Errors["listing-maintenance"], storage.ErrChecksumMismatch) {
		t.Fatal("repair accepted corrupt winner", result)
	}
	repairs, err := local.ListingRepairs(32)
	if err != nil || len(repairs) != 1 {
		t.Fatal("repair intent lost", repairs, err)
	}
	hints, err := remote.List(t.Context(), listingindex.V3Prefix)
	if err != nil || len(hints) != 0 {
		t.Fatal("corrupt authoritative source was indexed", hints, err)
	}
}

type publicationContextChange string

const (
	publicationDestinationChange publicationContextChange = "destination"
	publicationAdapterChange     publicationContextChange = "adapter"
	publicationFilterChange      publicationContextChange = "filter"
)

func TestPublicationChangedDestinationOrPrivacyContextRetainsPending(t *testing.T) {
	for _, mode := range []publicationContextChange{publicationDestinationChange, publicationAdapterChange, publicationFilterChange} {
		t.Run(string(mode), func(t *testing.T) {
			local := newTestStore(t)
			reg := registration(t, writeTranscript(t, t.TempDir(), "synthetic.jsonl", codexTranscript))
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
			at := reg.RegisteredAt.Add(time.Hour)
			opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic-machine", Now: func() time.Time { return at }}
			result, err := Run(t.Context(), local, remote, opts)
			if err != nil || result.Errors[reg.ArchiveSessionID] == nil {
				t.Fatal(result, err)
			}
			pending, found, err := local.LoadPending(reg.ArchiveSessionID)
			if err != nil || !found {
				t.Fatal(found, err)
			}
			switch mode {
			case publicationDestinationChange:
				reg.DestinationID = "different-destination"
			case publicationAdapterChange:
				pending.Bundle.Capture.AdapterVersion = "older-adapter-policy"
			case publicationFilterChange:
				pending.Bundle.Capture.FilterVersion = "older-filter-policy"
			}
			published, err := local.LoadPublishedState(reg.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			remote.failMetadata = false
			scan := newSessionScan(t.Context(), local, remote, reg, state.Request{}, published, at, opts)
			if _, err := scan.publishPending(pending); err == nil {
				t.Fatal("changed context admitted old bytes")
			}
			if _, err := remote.Get(t.Context(), pending.MetadataKey); !errors.Is(err, storage.ErrNotFound) {
				t.Fatal("changed context became authoritative", err)
			}
			if found, err := local.HasPending(reg.ArchiveSessionID); err != nil || !found {
				t.Fatal("changed context lost evidence", found, err)
			}
		})
	}
}

func TestPublicationMalformedJournalBlocksScanAndRemainsOutstanding(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "synthetic.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	at := reg.RegisteredAt.Add(time.Hour)
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", at); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(local.Home(), "pending", reg.ArchiveSessionID+".json")
	body := []byte(`{"bundle":{"sche`)
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	remote := &putCountingStore{MemoryStore: storagetest.NewMemoryStore()}
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic-machine", Now: func() time.Time { return at }}
	for range 2 {
		result, err := Run(t.Context(), local, remote, opts)
		if err != nil || result.Errors[reg.ArchiveSessionID] == nil || len(result.Published) != 0 || remote.takePuts() != 0 {
			t.Fatal("malformed journal became absent work", result, err)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != string(body) {
			t.Fatal("journal evidence changed", err)
		}
		if _, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || !found {
			t.Fatal("request completed prematurely", found, err)
		}
		work, err := local.Outstanding(reg, true)
		if err != nil || !work.Upload || !work.Pending() || !work.DefersExpiry() {
			t.Fatal("corrupt admitted evidence disappeared from work status", work, err)
		}
	}
}

func TestPublicationLostPublishedCacheCannotGuessPredecessor(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "synthetic.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := &putCountingStore{MemoryStore: storagetest.NewMemoryStore()}
	at := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic-machine", Now: func() time.Time { return at }}
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || len(result.Published) != 1 {
		t.Fatal(result, err)
	}
	path := filepath.Join(local.Home(), "published", reg.ArchiveSessionID+".json")
	if err := os.WriteFile(path, []byte(`{"bundle":{"sche`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", at); err != nil {
		t.Fatal(err)
	}
	key := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID).SourceBundle.Key
	before := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	remote.takePuts()
	for range 3 {
		at = at.Add(time.Hour)
		result, err := Run(t.Context(), local, remote, opts)
		if err != nil || result.Errors[reg.ArchiveSessionID] == nil || len(result.Published) != 0 {
			t.Fatal("lost exact predecessor guessed from remote", result, err)
		}
	}
	after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if after.SourceBundle.Key != key || !after.MetadataDerivedAt.Equal(before.MetadataDerivedAt) {
		t.Fatal("lost predecessor replaced authoritative metadata")
	}
	if len(local.QuarantinedFiles()) != 1 {
		t.Fatal("damaged published evidence not retained")
	}
	if _, found, err := local.LoadPending(reg.ArchiveSessionID); err != nil || !found {
		t.Fatal("replacement evidence not pending", found, err)
	}
}

func TestPublicationNewRequestCannotReplaceObsoletePendingEvidence(t *testing.T) {
	for _, mode := range []publicationContextChange{publicationAdapterChange, publicationFilterChange, publicationDestinationChange} {
		t.Run(string(mode), func(t *testing.T) {
			local := newTestStore(t)
			reg := registration(t, writeTranscript(t, t.TempDir(), "synthetic.jsonl", codexTranscript))
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
			at := reg.RegisteredAt.Add(time.Hour)
			opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic-machine", Now: func() time.Time { return at }}
			if result, err := Run(t.Context(), local, remote, opts); err != nil || result.Errors[reg.ArchiveSessionID] == nil {
				t.Fatal(result, err)
			}
			pending, found, err := local.LoadPending(reg.ArchiveSessionID)
			if err != nil || !found {
				t.Fatal(found, err)
			}
			pending.Attempted = false
			switch mode {
			case publicationAdapterChange, publicationFilterChange:
				// Legacy unattempted journals must also survive policy changes.
				pending.Commit = nil
				pending.Sources = nil
				if mode == publicationAdapterChange {
					pending.Bundle.Capture.AdapterVersion = "obsolete"
				} else {
					pending.Bundle.Capture.FilterVersion = "obsolete"
				}
			case publicationDestinationChange:
				reg.DestinationID = "changed-destination"
				if err := local.SaveRegistration(reg); err != nil {
					t.Fatal(err)
				}
			}
			if err := local.SavePending(reg.ArchiveSessionID, pending); err != nil {
				t.Fatal(err)
			}
			pendingPath := filepath.Join(local.Home(), "pending", reg.ArchiveSessionID+".json")
			before, err := os.ReadFile(pendingPath)
			if err != nil {
				t.Fatal(err)
			}
			at = at.Add(time.Hour)
			if err := local.SaveRequest(reg.ArchiveSessionID, "stop", at); err != nil {
				t.Fatal(err)
			}
			remote.failMetadata = false
			result, err := Run(t.Context(), local, remote, opts)
			if err != nil || result.Errors[reg.ArchiveSessionID] == nil || len(result.Published) != 0 {
				t.Fatal("new request replaced incompatible pending evidence", result, err)
			}
			after, err := os.ReadFile(pendingPath)
			if err != nil || string(before) != string(after) {
				t.Fatal("pending evidence changed", err)
			}
			if _, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || !found {
				t.Fatal("request acknowledged", found, err)
			}
		})
	}
}

type winnerAfterUploadStore struct {
	*storagetest.MemoryStore
	metadataKey   string
	readsAfterPut int
	winner        []byte
}

func (s *winnerAfterUploadStore) Put(ctx context.Context, key string, body []byte) error {
	if key == s.metadataKey {
		s.readsAfterPut = 1
	}
	return s.MemoryStore.Put(ctx, key, body)
}

func (s *winnerAfterUploadStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if key == s.metadataKey && s.readsAfterPut > 0 {
		s.readsAfterPut++
		if s.readsAfterPut == 3 {
			body, err := s.MemoryStore.GetLimited(ctx, key, limit)
			if err != nil {
				return nil, err
			}
			var m archive.Metadata
			if err := json.Unmarshal(body, &m); err != nil {
				return nil, err
			}
			m.SourceBundle.SHA256 = storage.SHA256Hex([]byte("missing authoritative source"))
			m.SourceBundle.Key = "sessions/claude/session-1/source." + m.SourceBundle.SHA256 + ".jsonl.gz"
			s.winner, err = json.Marshal(m)
			if err != nil {
				return nil, err
			}
			if err := s.MemoryStore.Put(ctx, key, s.winner); err != nil {
				return nil, err
			}
		}
	}
	return s.MemoryStore.GetLimited(ctx, key, limit)
}

func TestPublicationChangedWinnerIsNotIndexedWithoutSourceVerification(t *testing.T) {
	local := newTestStore(t)
	claudeSession(t, local, claudePromptLine+"\n")
	remote := &winnerAfterUploadStore{MemoryStore: storagetest.NewMemoryStore(), metadataKey: "sessions/claude/session-1/metadata.json"}
	at := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	result := runAt(t, local, remote, at)
	if !errors.Is(result.Errors["session-1"], storage.ErrPublicationConflict) || len(result.Published) != 0 {
		t.Fatal(result)
	}
	hints, err := remote.List(t.Context(), listingindex.V3Prefix)
	if err != nil || len(hints) != 0 {
		t.Fatal("unverified winner indexed", hints, err)
	}
	repairs, err := local.ListingRepairs(32)
	if err != nil || len(repairs) != 1 {
		t.Fatal("winner repair evidence lost", repairs, err)
	}
	result = runAt(t, local, remote, at.Add(time.Hour))
	if !errors.Is(result.Errors["listing-maintenance"], storage.ErrNotFound) {
		t.Fatal("winner source not checked on repair", result)
	}
}

type retainedComparatorSources struct {
	agentapi.SourcesLookup
	filter agentapi.TranscriptFilter
}

func (s retainedComparatorSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	provider, _, ok := s.SourcesLookup.LookupSources(name)
	return provider, s.filter, ok
}

type recordingRetainedFilter struct {
	agentapi.TranscriptFilter
	allow bool
	calls int
}

func (f *recordingRetainedFilter) EvidenceExtends(previous, candidate archive.SourceBundle) bool {
	f.calls++
	return f.allow && f.TranscriptFilter.EvidenceExtends(previous, candidate)
}

func historyPublication(t *testing.T, reg archive.SessionRegistration, at time.Time) state.PendingPublication {
	t.Helper()
	adapter, err := sourceAdapter(testSources, "codex")
	if err != nil {
		t.Fatal(err)
	}
	b := archive.SourceBundle{SchemaVersion: archive.HistorySourceSchemaVersion, ArchiveSessionID: reg.ArchiveSessionID, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, Capture: archive.SourceCapture{Harness: reg.Harness, AdapterName: "codex", AdapterVersion: adapter.Version(), FilterVersion: archive.FilterVersion, CapturedAt: at, SourceFormat: "codex-jsonl"}, NativeRecords: []map[string]any{{"type": "event_msg", "payload": map[string]any{"type": "task_started"}}}, Ordinals: []uint64{1}, History: &archive.SourceHistory{ActiveRolloutID: reg.NativeSessionID, ThreadID: reg.NativeSessionID, Spans: []archive.HistorySpan{{RolloutID: reg.NativeSessionID, ThreadID: reg.NativeSessionID, EndRecord: 1, StartOrdinal: 1, EndOrdinal: 2}}}}
	compressed, err := archive.BuildCompressedSource(b)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.SourceObjectKey(b, compressed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	ref := archive.SourceReference{Key: key, SHA256: compressed.SHA256, CompressedBytes: len(compressed.Bytes)}
	m, err := archive.BuildMetadataWithAnalysis(b, archive.Analysis{}, nil, "synthetic-machine", at, at, ref, archive.ParserInfo{Version: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	metadataKey, err := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	return state.PendingPublication{Bundle: b, SourceKey: key, SourceSHA256: ref.SHA256, SourceBytes: compressed.Bytes, MetadataKey: metadataKey, MetadataBytes: raw}
}

func TestPublicationSealingUsesInjectedRetainedComparatorWithoutEnablingWriter(t *testing.T) {
	for _, allow := range []bool{true, false} {
		t.Run(strconv.FormatBool(allow), func(t *testing.T) {
			local := newTestStore(t)
			reg := registration(t, writeTranscript(t, t.TempDir(), "synthetic.jsonl", codexTranscript))
			reg.NativeSessionID = "11111111-1111-4111-8111-111111111111"
			at := reg.RegisteredAt.Add(time.Hour)
			old := historyPublication(t, reg, at)
			sealed, err := state.PreparePublication(old, state.PublicationPredecessor{State: state.PredecessorAbsent}, "", "a", "p", state.PublicationCapture)
			if err != nil {
				t.Fatal(err)
			}
			published, err := local.LoadPublishedState(reg.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			if err := published.SaveCommittedPublication(sealed, at); err != nil {
				t.Fatal(err)
			}
			next := historyPublication(t, reg, at.Add(time.Hour))
			adapter, err := sourceAdapter(testSources, "codex")
			if err != nil {
				t.Fatal(err)
			}
			filter := &recordingRetainedFilter{TranscriptFilter: adapter, allow: allow}
			remote := storagetest.NewMemoryStore()
			scan := newSessionScan(t.Context(), local, remote, reg, state.Request{}, published, at, Options{Sources: retainedComparatorSources{SourcesLookup: testSources, filter: filter}})
			err = scan.sealPending(&next)
			if (err == nil) != allow || filter.calls != 1 {
				t.Fatal("comparator decision not honored", err, filter.calls)
			}
			if allow {
				if next.Commit == nil || next.ValidatePublication() != nil {
					t.Fatal("continuity not sealed")
				}
				if _, err := scan.publishPending(next); !errors.Is(err, archive.ErrHistoryMutationPending) {
					t.Fatal("writer fence lifted", err)
				}
			}
		})
	}
}

type oversizedPublicationStore struct {
	*storagetest.MemoryStore
	unlimitedReads int
	limitedReads   int
}

func (s *oversizedPublicationStore) Get(context.Context, string) ([]byte, error) {
	s.unlimitedReads++
	return nil, errors.New("unbounded authoritative metadata read")
}

func (s *oversizedPublicationStore) GetLimited(_ context.Context, _ string, limit int64) ([]byte, error) {
	s.limitedReads++
	if limit != 32<<20 {
		return nil, errors.New("incorrect authoritative metadata bound")
	}
	return nil, storage.ErrObjectTooLarge
}

func TestPublicationRecoveryMetadataReadsUseAllocationBound(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "synthetic.jsonl", codexTranscript))
	remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
	at := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic-machine", Now: func() time.Time { return at }}
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if result, err := Run(t.Context(), local, remote, opts); err != nil || result.Errors[reg.ArchiveSessionID] == nil {
		t.Fatal(result, err)
	}
	pending, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	published, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := published.SaveCommittedPublication(pending, at); err != nil {
		t.Fatal(err)
	}
	if err := published.CacheMetadata(nil); err != nil {
		t.Fatal(err)
	}
	bounded := &oversizedPublicationStore{MemoryStore: storagetest.NewMemoryStore()}
	scan := newSessionScan(t.Context(), local, bounded, reg, state.Request{}, published, at, opts)
	if err := scan.checkHistoryPublication(pending); !errors.Is(err, storage.ErrObjectTooLarge) {
		t.Fatal(err)
	}
	if _, usable := scan.lastPublication(pending.MetadataKey); usable {
		t.Fatal("oversized legacy metadata adopted")
	}
	if bounded.unlimitedReads != 0 || bounded.limitedReads != 2 {
		t.Fatal("recovery bypassed metadata allocation bound", bounded.unlimitedReads, bounded.limitedReads)
	}
}
