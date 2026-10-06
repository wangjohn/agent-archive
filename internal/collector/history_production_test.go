package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type historyCrashStore struct {
	*storagetest.MemoryStore
	calls    int
	fail     int
	after    bool
	puts     int
	afterPut func(string)
}

func (s *historyCrashStore) boundary(key string) bool {
	if !strings.HasPrefix(key, "sessions/") {
		return false
	}
	s.calls++
	return s.fail > 0 && s.calls == s.fail
}

func (s *historyCrashStore) Put(ctx context.Context, key string, raw []byte) error {
	crash := s.boundary(key)
	if crash && !s.after {
		return errors.New("synthetic publication interruption")
	}
	if err := s.MemoryStore.Put(ctx, key, raw); err != nil {
		return err
	}
	if strings.HasPrefix(key, "sessions/") {
		s.puts++
	}
	if s.afterPut != nil {
		s.afterPut(key)
	}
	if crash {
		return errors.New("synthetic interrupted PUT response")
	}
	return nil
}

func (s *historyCrashStore) GetLimited(ctx context.Context, key string, n int64) ([]byte, error) {
	if s.boundary(key) {
		return nil, errors.New("synthetic bounded read interruption")
	}
	return s.MemoryStore.GetLimited(ctx, key, n)
}

func (s *historyCrashStore) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if s.boundary(key) {
		return storage.ObjectInfo{}, errors.New("synthetic verification interruption")
	}
	return s.MemoryStore.Stat(ctx, key)
}

func runHistoryRetry(t *testing.T, scan *sessionScan, remote storage.ObjectStore) Result {
	t.Helper()
	reopened, err := state.Open(scan.local.Home())
	if err != nil {
		t.Fatal(err)
	}
	scan.local = reopened
	opts := scan.opts
	opts.RepoKey = func(string) string { t.Fatal("frozen publication read current Git"); return "" }
	opts.SupplementalEvidence = func(archive.SessionRegistration, time.Time) ([]archive.SupplementalEvidence, error) {
		t.Fatal("frozen publication read current inventory")
		return nil, nil
	}
	opts.Retry = storage.RetryPolicy{MaxAttempts: 1}
	result, err := Run(t.Context(), reopened, remote, opts)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertCompleteHistory(t *testing.T, scan *sessionScan, p state.PendingPublication, remote storage.ObjectStore) {
	t.Helper()
	raw, err := remote.Get(t.Context(), p.MetadataKey)
	if err != nil || !bytes.Equal(raw, p.MetadataBytes) {
		t.Fatal("final bytes changed", err)
	}
	var m archive.Metadata
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	refs, err := m.SourceReferences()
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		raw, err := remote.Get(t.Context(), ref.Key)
		if err != nil {
			t.Fatal("dangling final metadata", err)
		}
		if _, err := decodeHistoryStage(t.Context(), m, p, ref, raw); err != nil {
			t.Fatal(err)
		}
	}
	if _, found, err := scan.local.LoadPending(scan.id()); err != nil || found {
		t.Fatal("journal not cleaned", err)
	}
}

func TestRunHistoryPublicationReopensAtEveryRemoteBoundary(t *testing.T) {
	baseline, p := privacyJournal(t)
	cloud := &historyCrashStore{MemoryStore: baseline.remote.(*storagetest.MemoryStore)}
	result := runHistoryRetry(t, baseline, cloud)
	if len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result)
	}
	assertCompleteHistory(t, baseline, p, cloud)
	boundaries := cloud.calls
	for _, after := range []bool{false, true} {
		for fail := 1; fail <= boundaries; fail++ {
			t.Run(fmt.Sprintf("%d/after=%v", fail, after), func(t *testing.T) {
				scan, p := privacyJournal(t)
				if err := scan.local.SaveRequest(scan.id(), "newer", scan.now); err != nil {
					t.Fatal(err)
				}
				newer, _, err := scan.local.LoadRequest(scan.id())
				if err != nil {
					t.Fatal(err)
				}
				// Native input is gone before any final remote operation.
				entries, err := os.ReadDir(scan.reg.ProjectRoot)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if strings.HasSuffix(entry.Name(), ".jsonl") {
						if err := os.Remove(filepath.Join(scan.reg.ProjectRoot, entry.Name())); err != nil {
							t.Fatal(err)
						}
					}
				}
				remote := &historyCrashStore{MemoryStore: scan.remote.(*storagetest.MemoryStore), fail: fail, after: after}
				first := runHistoryRetry(t, scan, remote)
				if len(first.Errors) == 0 {
					t.Fatal("fail point did not stop publication", fail, first)
				}
				if _, found, err := scan.local.LoadPending(scan.id()); err != nil || !found {
					// Auxiliary listing failure retains its own durable repair;
					// exact source/metadata acknowledgement may already be complete.
					if err != nil || len(first.Published) != 1 || !strings.Contains(first.Errors[scan.id()].Error(), "listing maintenance pending") {
						t.Fatal("lost durable retry", err, first.Errors)
					}
					assertCompleteHistory(t, scan, p, remote)
					repairs, err := scan.local.ListingRepairs(32)
					if err != nil || len(repairs) != 1 {
						t.Fatal("listing retry lost", err)
					}
					remote.fail = 0
					pass := &pass{ctx: t.Context(), local: scan.local, remote: remote, result: Result{Errors: map[string]error{}}}
					pass.repairListingIndex()
					if len(pass.result.Errors) != 0 {
						t.Fatal(pass.result.Errors)
					}
					return
				}
				// If a final sidecar is visible at interruption, every ref must exist.
				if raw, err := remote.Get(t.Context(), p.MetadataKey); err == nil {
					var m archive.Metadata
					if err := json.Unmarshal(raw, &m); err != nil {
						t.Fatal(err)
					}
					refs, err := m.SourceReferences()
					if err != nil {
						t.Fatal(err)
					}
					for _, ref := range refs {
						if raw, err := remote.Get(t.Context(), ref.Key); err != nil || !storage.VerifySHA256(raw, ref.SHA256) {
							t.Fatal("dangling metadata", err)
						}
					}
				}
				committedRaw, _ := remote.Get(t.Context(), p.MetadataKey)
				committed := bytes.Equal(committedRaw, p.MetadataBytes)
				puts := remote.puts
				remote.fail = 0
				final := runHistoryRetry(t, scan, remote)
				if len(final.Errors) != 0 || len(final.Published) != 1 {
					t.Fatal("retry failed", final)
				}
				if committed && remote.puts != puts {
					t.Fatal("committed retry performed a needless PUT")
				}
				assertCompleteHistory(t, scan, p, remote)
				request, found, err := scan.local.LoadRequest(scan.id())
				if err != nil || !found || request.Token != newer.Token {
					t.Fatal("newer token lost", err)
				}
			})
		}
	}
}

func TestRunCommittedHistoryRetryDoesNotRequireRemovedPrivateStages(t *testing.T) {
	scan, p := privacyJournal(t)
	cloud := scan.remote.(*storagetest.MemoryStore)
	if err := scan.upload(p); err != nil {
		t.Fatal(err)
	}
	for _, stage := range p.History.Sources {
		if err := os.Remove(filepath.Join(scan.local.Home(), "sessions", scan.id(), "pending-sources", stage.Name)); err != nil {
			t.Fatal(err)
		}
	}
	remote := &historyCrashStore{MemoryStore: cloud}
	result := runHistoryRetry(t, scan, remote)
	if len(result.Errors) != 0 || len(result.Published) != 1 || remote.puts != 0 {
		t.Fatal(result, remote.puts)
	}
	assertCompleteHistory(t, scan, p, remote)
}

func TestRunCommittedHistoryCleanupFailuresRetainRetry(t *testing.T) {
	type pointVariant0 string
	const (
		pointRetiredLedger0 pointVariant0 = "retired-ledger"
		pointCache0         pointVariant0 = "cache"
		pointRequest0       pointVariant0 = "request"
		pointPrivateStages0 pointVariant0 = "private-stages"
	)
	for _, point := range []pointVariant0{pointRetiredLedger0, pointCache0, pointRequest0, pointPrivateStages0} {
		t.Run(string(point), func(t *testing.T) {
			scan, p := privacyJournal(t)
			older := p.Bundle
			older.Capture.CapturedAt = older.Capture.CapturedAt.Add(-time.Hour)
			packed, err := archive.BuildCompressedSource(older)
			if err != nil {
				t.Fatal(err)
			}
			key, err := archive.SourceObjectKey(older, packed.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			retired := archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
			p.History.Retired = []state.RetiredSource{{Reference: retired, RetiredAt: scan.now, PrivacySensitive: true}}
			if err := scan.local.SaveRequest(scan.id(), "stop", scan.now); err != nil {
				t.Fatal(err)
			}
			req, _, err := scan.local.LoadRequest(scan.id())
			if err != nil {
				t.Fatal(err)
			}
			p.RequestToken = req.Token
			if err := scan.local.SavePending(scan.id(), p); err != nil {
				t.Fatal(err)
			}
			cloud := scan.remote.(*storagetest.MemoryStore)
			if err := cloud.Put(t.Context(), key, packed.Bytes); err != nil {
				t.Fatal(err)
			}
			path := ""
			remote := &historyCrashStore{MemoryStore: cloud}
			remote.afterPut = func(key string) {
				if key != p.MetadataKey {
					return
				}
				remote.afterPut = nil
				switch point {
				case pointRetiredLedger0:
					path = filepath.Join(scan.local.Home(), "superseded", scan.id()+".json")
				case pointCache0:
					path = publishedPath(scan.local, scan.id())
				case pointRequest0:
					path = filepath.Join(scan.local.Home(), "requests", scan.id()+".json")
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				case pointPrivateStages0:
					path = filepath.Join(scan.local.Home(), "sessions", scan.id(), "pending-sources", "unsafe-obligation")
				}
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			result := runHistoryRetry(t, scan, remote)
			if len(result.Errors) == 0 || len(result.Published) != 0 {
				t.Fatal("cleanup failure falsely completed", result)
			}
			if _, found, err := scan.local.LoadPending(scan.id()); err != nil || !found {
				t.Fatal("cleanup retry lost", err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if point == pointRequest0 {
				if err := scan.local.SaveRequest(scan.id(), "newer", scan.now.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			puts := remote.puts
			final := runHistoryRetry(t, scan, remote)
			if len(final.Errors) != 0 || len(final.Published) != 1 || remote.puts != puts {
				t.Fatal("committed cleanup retry", final, remote.puts, puts)
			}
			assertCompleteHistory(t, scan, p, remote)
			ledger, err := scan.local.LoadSuperseded(scan.id())
			if err != nil || len(ledger) != 1 || ledger[0].Key != retired.Key || !ledger[0].PrivacySensitive || !ledger[0].SupersededAt.Equal(scan.now) {
				t.Fatal("retirement lost", ledger, err)
			}
			if point == pointRequest0 {
				if _, found, err := scan.local.LoadRequest(scan.id()); err != nil || !found {
					t.Fatal("newer request lost", err)
				}
			}
		})
	}
}

func TestRunHistorySettlesWithoutPublishedDecodesOrSessionWrites(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	opts := scan.opts
	opts.RepoKey = func(string) string { return "" }
	for range 5 {
		result, err := Run(t.Context(), scan.local, scan.remote, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, err := range result.Errors {
			if !errors.Is(err, archive.ErrHistoryMutationPending) {
				t.Fatal(result.Errors)
			}
		}
	}
	if _, found, err := scan.local.LoadScanSignature(scan.id()); err != nil || !found {
		t.Fatal("complete history did not settle", err)
	}
	result, cost := measurePass(t, scan.local, scan.remote, opts)
	if len(result.Errors) != 0 || len(result.Published) != 0 || cost.loads != 0 || cost.writes != 0 {
		t.Fatal("settled history work", result, cost)
	}
}

func TestRunHistoryRecoveryFrozenParserAndHookMaintenanceUseRetainedSet(t *testing.T) {
	scan, p := privacyJournal(t)
	if result := runHistoryRetry(t, scan, scan.remote); len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result)
	}
	published, err := scan.local.LoadPublishedState(scan.id())
	if err != nil {
		t.Fatal(err)
	}
	if err := published.SaveBlocked(p.Bundle, scan.now, state.BlockedReasonTranscriptRewritten); err != nil {
		t.Fatal(err)
	}
	opts := scan.opts
	opts.RepoKey = func(string) string { return "" }
	build, err := PrepareGenerationRecovery(t.Context(), scan.reg, scan.now.Add(time.Hour), opts)
	if err != nil {
		t.Fatal(err)
	}
	successor, err := scan.local.BeginGenerationRecovery(scan.id(), scan.now.Add(time.Hour), build)
	if err != nil {
		t.Fatal(err)
	}
	pending, found, err := scan.local.LoadPending(successor)
	if err != nil || !found || pending.History == nil || pending.Bundle.History == nil {
		t.Fatal("history recovery not journaled", err)
	}
	// The frozen original has all three independent references; successor owns
	// one current self-contained snapshot under its own generation prefix.
	if err := os.RemoveAll(scan.reg.ProjectRoot); err != nil {
		t.Fatal(err)
	}
	frozen, found, err := scan.local.LoadRegistration(scan.id())
	if err != nil || !found || !frozen.CaptureFrozen {
		t.Fatal("original not frozen", err)
	}
	opts.AcceptSession = func(reg archive.SessionRegistration) bool { return reg.ArchiveSessionID == scan.id() }
	opts.ParserVersion = "synthetic-frozen-parser-maintenance"
	opts.CodexRollouts = nil
	opts.RepoKey = func(string) string { t.Fatal("frozen source3 read current Git"); return "" }
	opts.SupplementalEvidence = func(archive.SessionRegistration, time.Time) ([]archive.SupplementalEvidence, error) {
		t.Fatal("frozen source3 read current inventory")
		return nil, nil
	}
	evidence := archive.SupplementalEvidence{Kind: archive.EvidenceKindExplicitFeedback, Provenance: "synthetic-hook", ObservedAt: scan.now.Add(time.Hour), Payload: map[string]any{"text": "synthetic retained feedback"}}
	if err := scan.local.SaveRequest(scan.id(), "feedback", scan.now.Add(time.Hour), evidence); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		scan.local, err = state.Open(scan.local.Home())
		if err != nil {
			t.Fatal(err)
		}
		result, err := Run(t.Context(), scan.local, scan.remote, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, err := range result.Errors {
			if !errors.Is(err, archive.ErrHistoryMutationPending) {
				t.Fatal(result.Errors)
			}
		}
	}
	raw, err := scan.remote.Get(t.Context(), p.MetadataKey)
	if err != nil {
		t.Fatal(err)
	}
	var m archive.Metadata
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Parser.Version != opts.ParserVersion || len(m.History.Preserved) != 2 || !m.CapturedAt.Equal(p.Bundle.Capture.CapturedAt) {
		t.Fatal("frozen manifest/capture lost", m.History)
	}
	data, err := scan.remote.Get(t.Context(), m.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := reader.DecodeReferencedSource(t.Context(), m, data, reader.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("feedback")) && len(b.SupplementalEvidence) == 0 {
		t.Fatal("owed observation lost")
	}
	if _, found, err := scan.local.LoadRequest(scan.id()); err != nil || found {
		t.Fatal("frozen observation not covered", err)
	}
	result, cost := measurePass(t, scan.local, scan.remote, opts)
	if len(result.Errors) != 0 || cost.loads != 0 || cost.writes != 0 {
		t.Fatal("settled frozen history work", result, cost)
	}
}

func TestRunMixedHistoryPrivacyRemovesSecretsFromEveryAlternative(t *testing.T) {
	scan, p := privacyJournal(t)
	var m archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &m); err != nil {
		t.Fatal(err)
	}
	const secret = "sk-abcdefghijklmnopqrstuv"
	for i, input := range p.History.Inputs {
		b, err := scan.loadHistoryInput(m, input)
		if err != nil {
			t.Fatal(err)
		}
		b.Capture.FilterVersion = "14"
		if i == 1 {
			b.SchemaVersion = archive.SourceSchemaVersion
			b.History = nil
			b.Ordinals = nil
		}
		for _, record := range b.NativeRecords {
			record["api_key"] = secret
		}
		packed, err := archive.BuildCompressedSource(b)
		if err != nil {
			t.Fatal(err)
		}
		key, err := archive.SourceObjectKey(b, packed.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		ref := archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
		if err := scan.remote.Put(t.Context(), key, packed.Bytes); err != nil {
			t.Fatal(err)
		}
		if input.RevisionID == m.History.CurrentRevision {
			m.SourceBundle = ref
			m.FilterVersion = "14"
			p.Bundle = b
		} else {
			for n := range m.History.Preserved {
				r := &m.History.Preserved[n]
				if r.RevisionID == input.RevisionID {
					r.Source = ref
					r.SourceSchemaVersion = b.SchemaVersion
					r.FilterVersion = "14"
				}
			}
		}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := scan.remote.Put(t.Context(), p.MetadataKey, raw); err != nil {
		t.Fatal(err)
	}
	if err := scan.published.SavePublication(p.Bundle, scan.now, m.SourceBundle, raw); err != nil {
		t.Fatal(err)
	}
	if err := scan.local.RemovePending(scan.id()); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(scan.reg.ProjectRoot); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		result := runHistoryRetry(t, scan, scan.remote)
		for _, err := range result.Errors {
			if !errors.Is(err, archive.ErrHistoryMutationPending) {
				t.Fatal(result.Errors)
			}
		}
	}
	final, err := scan.remote.Get(t.Context(), p.MetadataKey)
	if err != nil {
		t.Fatal(err)
	}
	var clean archive.Metadata
	if err := json.Unmarshal(final, &clean); err != nil {
		t.Fatal(err)
	}
	refs, err := clean.SourceReferences()
	if err != nil || len(refs) != 3 {
		t.Fatal(refs, err)
	}
	formats := map[int]bool{}
	for _, ref := range refs {
		raw, err := scan.remote.Get(t.Context(), ref.Key)
		if err != nil {
			t.Fatal(err)
		}
		b, err := decodeHistoryStage(t.Context(), clean, p, ref, raw)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(encoded, []byte(secret)) || bytes.Contains(encoded, []byte("api_key")) {
			t.Fatal("complete privacy claim retained an alternative secret")
		}
		if !b.Capture.CapturedAt.Equal(scan.now) || b.Capture.FilterVersion != archive.FilterVersion {
			t.Fatal("capture changed or incomplete privacy")
		}
		formats[b.SchemaVersion] = true
		if err := b.ValidateHistory(); err != nil {
			t.Fatal("raw ordinal graph invalid", err)
		}
	}
	if !formats[archive.SourceSchemaVersion] || !formats[archive.HistorySourceSchemaVersion] {
		t.Fatal("mixed formats were flattened")
	}
}

func TestRunReactivationProtectsEveryReferenceAndPreservesMeaningfulCapture(t *testing.T) {
	scan, lookup := reconciliationFixture(t)
	opts := scan.opts
	opts.RepoKey = func(string) string { return "" }
	for range 3 {
		result, err := Run(t.Context(), scan.local, scan.remote, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, err := range result.Errors {
			if !errors.Is(err, archive.ErrHistoryMutationPending) {
				t.Fatal(result.Errors)
			}
		}
	}
	m := fetchMetadata(t, scan.remote, "codex", scan.id())
	var retained archive.RevisionReference
	for _, revision := range m.History.Preserved {
		if revision.RevisionID == revisionB {
			retained = revision
		}
	}
	if retained.RevisionID == "" {
		t.Fatal("B not retained")
	}
	current := lookup.refs[revisionB][0]
	lookup.set.Current = &current
	lookup.set.Revision = "reactivated-B"
	opts.Now = func() time.Time { return scan.now.Add(time.Hour) }
	for range 3 {
		result, err := Run(t.Context(), scan.local, scan.remote, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, err := range result.Errors {
			if !errors.Is(err, archive.ErrHistoryMutationPending) {
				t.Fatal(result.Errors)
			}
		}
	}
	final := fetchMetadata(t, scan.remote, "codex", scan.id())
	if final.History.CurrentRevision != revisionB || final.SourceBundle != retained.Source || !final.CapturedAt.Equal(retained.CapturedAt) {
		t.Fatal("reactivation invented capture or changed exact bytes", final.SourceBundle, final.CapturedAt)
	}
	refs, err := final.SourceReferences()
	if err != nil {
		t.Fatal(err)
	}
	protected := map[string]bool{}
	for _, ref := range refs {
		protected[ref.Key] = true
	}
	ledger, err := scan.local.LoadSuperseded(scan.id())
	if err != nil {
		t.Fatal(err)
	}
	for _, retired := range ledger {
		if protected[retired.Key] {
			t.Fatal("reactivated reference retired")
		}
	}
}
