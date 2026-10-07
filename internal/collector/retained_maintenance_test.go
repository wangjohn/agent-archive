package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

const maintenanceThread = "11111111-1111-4111-8111-111111111111"
const maintenanceRevision = "22222222-2222-4222-8222-222222222222"

func retainedHistoryFixture(t *testing.T) (*sessionScan, archive.Metadata, []byte) {
	t.Helper()
	local := newTestStore(t)
	remote := storagetest.NewMemoryStore()
	at := time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC)
	reg := registration(t, "deleted-synthetic-native")
	reg.NativeSessionID = maintenanceThread
	reg.SessionStartedAt = at.Add(-time.Hour)
	reg.RegisteredAt = at
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	makeSource := func(revision string, captured time.Time) (archive.SourceBundle, archive.SourceReference) {
		b := archive.SourceBundle{SchemaVersion: archive.HistorySourceSchemaVersion, ArchiveSessionID: reg.ArchiveSessionID, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, Capture: archive.SourceCapture{Harness: reg.Harness, AdapterName: "codex", AdapterVersion: "obsolete-adapter", FilterVersion: "obsolete-filter", SourceFormat: "codex-jsonl", CapturedAt: captured}, History: &archive.SourceHistory{ThreadID: maintenanceThread, ActiveRolloutID: revision, Spans: []archive.HistorySpan{{RolloutID: revision, ThreadID: maintenanceThread, StartOrdinal: 0, EndOrdinal: 3, EndRecord: 3}}}, Ordinals: []uint64{0, 1, 2}, NativeRecords: []map[string]any{{"type": "session_meta", "payload": map[string]any{"id": maintenanceThread, "cwd": "/synthetic/deleted", "timestamp": "2026-10-01T12:00:00Z", "cli_version": "0.160.0", "originator": "codex_cli_rs", "source": "cli", "history_mode": "paginated"}}, {"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "synthetic retained prompt"}}, {"type": "unknown_native_type", "secret": "obsolete-private-marker"}}}
		encoded, err := archive.BuildCompressedSource(b)
		if err != nil {
			t.Fatal(err)
		}
		key, err := archive.SourceObjectKey(b, encoded.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		ref := archive.SourceReference{Key: key, SHA256: encoded.SHA256, CompressedBytes: len(encoded.Bytes)}
		if err := remote.Put(t.Context(), key, encoded.Bytes); err != nil {
			t.Fatal(err)
		}
		return b, ref
	}
	prior, priorRef := makeSource(maintenanceThread, at)
	current, currentRef := makeSource(maintenanceRevision, at.Add(2*time.Hour))
	m, err := archive.BuildMetadataWithAnalysis(current, archive.Analysis{}, nil, "synthetic-machine", reg.SessionStartedAt, at.Add(2*time.Hour), currentRef, archive.ParserInfo{Version: "old-parser"})
	if err != nil {
		t.Fatal(err)
	}
	m.History.Preserved = []archive.RevisionReference{{RevisionID: maintenanceThread, CapturedAt: prior.Capture.CapturedAt, Source: priorRef}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	published, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := published.SavePublication(current, at, currentRef, raw); err != nil {
		t.Fatal(err)
	}
	key, _ := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if err := remote.Put(t.Context(), key, raw); err != nil {
		t.Fatal(err)
	}
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic-machine", SkillEvidence: config.SkillEvidenceNone}
	return newSessionScan(t.Context(), local, remote, reg, state.Request{}, published, at.Add(4*time.Hour), opts), m, raw
}

func TestRetainedPrivacyTransformsEveryRevisionWithoutNativeAndPreservesAge(t *testing.T) {
	s, m, raw := retainedHistoryFixture(t)
	pending, err := s.prepareRetainedPrivacy(raw, remoteRetainedLoader(s.remote, m), s.published.PublicationPredecessor(), state.PrivacyCommitted, "", "", "body")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Commit.Purpose != state.PublicationPrivacyRewrite || len(pending.Sources) != 2 || len(pending.PrivacyRetiredSources()) != 2 {
		t.Fatal(pending.Commit)
	}
	var next archive.Metadata
	if err := json.Unmarshal(pending.MetadataBytes, &next); err != nil {
		t.Fatal(err)
	}
	if !next.CapturedAt.Equal(m.CapturedAt) || !next.History.Preserved[0].CapturedAt.Equal(m.History.Preserved[0].CapturedAt) || !next.StartedAt.Equal(m.StartedAt) {
		t.Fatal("privacy refreshed capture age")
	}
	for i, source := range pending.Sources {
		data := source.Bytes
		if i == 0 {
			data = pending.SourceBytes
		}
		b, err := archive.ReadSourceBundle(bytes.NewReader(data), archive.DecodeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(b)
		if bytes.Contains(encoded, []byte("obsolete-private-marker")) || b.Capture.FilterVersion != archive.FilterVersion || len(b.NativeRecords) != 2 {
			t.Fatal("preserved private record survived refilter", string(encoded))
		}
	}
	if err := s.local.SavePending(s.id(), pending); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(s.local.Home())
	if err != nil {
		t.Fatal(err)
	}
	replay, found, err := reopened.LoadPending(s.id())
	if err != nil || !found || replay.ValidatePublication() != nil {
		t.Fatal(found, err)
	}
	sources := make([]storage.SourcePublication, len(replay.Sources))
	for i, source := range replay.Sources {
		data := source.Bytes
		if i == 0 {
			data = replay.SourceBytes
		}
		sources[i] = storage.SourcePublication{Key: source.Reference.Key, SHA256: source.Reference.SHA256, Size: source.Reference.CompressedBytes, Bytes: data}
	}
	if err := storage.PutSourceSetThenMetadata(t.Context(), s.remote, sources, replay.MetadataKey, replay.MetadataBytes, storage.MetadataPredecessor{Known: true, Exists: true, SHA256: replay.Commit.PredecessorSHA256}, storage.RetryPolicy{}); err != nil {
		t.Fatal(err)
	}
	if err := s.published.SaveCommittedPublication(replay, s.now); err != nil {
		t.Fatal(err)
	}
	refs, err := s.published.CommittedSources()
	if err != nil || len(refs) != 2 {
		t.Fatal(refs, err)
	}
	for _, ref := range refs {
		if ref == m.SourceBundle || ref == m.History.Preserved[0].Source {
			t.Fatal("obsolete source remains reachable")
		}
	}
	if _, err := s.publishPending(replay); !errors.Is(err, archive.ErrHistoryMutationPending) {
		t.Fatal("history outer fence lifted", err)
	}
}

func TestRetainedRevisionLoaderRejectsMissingCorruptForeignAndCanceledEvidence(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt", "foreign", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			s, m, _ := retainedHistoryFixture(t)
			selection := m.History.Preserved[0]
			ctx := t.Context()
			switch mode {
			case "missing":
				if err := s.remote.Delete(ctx, selection.Source.Key); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := s.remote.Put(ctx, selection.Source.Key, []byte("broken")); err != nil {
					t.Fatal(err)
				}
			case "foreign":
				selection.CapturedAt = selection.CapturedAt.Add(time.Minute)
			case "canceled":
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			}
			if _, err := reader.LoadRevisionSource(ctx, s.remote, m, selection, reader.Limits{}); err == nil {
				t.Fatal("unsafe preserved source accepted")
			}
		})
	}
}

func TestRetainedParserRefreshPreservesCompleteSelectionAndCreation(t *testing.T) {
	s, m, _ := retainedHistoryFixture(t)
	current, err := reader.LoadSource(t.Context(), s.remote, m, reader.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	// Parser-only maintenance uses the same retained codec version and exact refs.
	s.opts.ParserVersion = "new-parser"
	s.opts.Sources = nil
	s.opts.Parsers = testParsers
	last := lastPublication{bundle: current, metadata: m}
	raw, changed, err := s.refreshedMetadata(last, m.SourceBundle)
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	var next archive.Metadata
	if err := json.Unmarshal(raw, &next); err != nil {
		t.Fatal(err)
	}
	if next.SourceBundle != m.SourceBundle || len(next.History.Preserved) != 1 || next.History.Preserved[0] != m.History.Preserved[0] || !next.StartedAt.Equal(m.StartedAt) || !next.CapturedAt.Equal(m.CapturedAt) {
		t.Fatal("parser refresh changed retained selection")
	}
}

func TestAdmissionStagePrivacyReceiptReleasesOnlyVerifiedReplacement(t *testing.T) {
	extra := archive.SupplementalEvidence{Kind: archive.EvidenceKindSkillSnapshot, Provenance: "synthetic", Payload: map[string]any{"name": "synthetic", "snapshot": "private-skill-body"}}
	local, reg := stagedPolicyFixture(t, config.SkillEvidenceBody, extra)
	manifest, bundle, err := local.ReadAdmissionStage(reg.ArchiveSessionID, reg.AdmissionStage)
	if err != nil {
		t.Fatal(err)
	}
	published, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	req, _, err := local.LoadRequest(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic-machine", SkillEvidence: config.SkillEvidenceNone}
	s := newSessionScan(t.Context(), local, remote, reg, req, published, reg.RegisteredAt.Add(time.Hour), opts)
	rendered, err := renderPublication(t.Context(), s.resolveParser(), s.parserVersion(), bundle, reg, s.now, opts, func() string { return reg.RepoKey })
	if err != nil {
		t.Fatal(err)
	}
	if rendered.source.SHA256 != manifest.SHA256 {
		t.Fatal("stage fixture differs")
	}
	loader := func(ctx context.Context, ref archive.RevisionReference) (archive.SourceBundle, error) {
		return bundle, ctx.Err()
	}
	pending, err := s.prepareRetainedPrivacy(rendered.metadata, loader, published.PublicationPredecessor(), state.PrivacyStage, reg.AdmissionStage, manifest.SHA256, manifest.SkillEvidence)
	if err != nil {
		t.Fatal(err)
	}
	if pending.SourceSHA256 == manifest.SHA256 {
		t.Fatal("skill reduction retained obsolete evidence")
	}
	if err := local.SavePending(reg.ArchiveSessionID, pending); err != nil {
		t.Fatal(err)
	}
	if _, err := s.publishPending(pending); err != nil {
		t.Fatal(err)
	}
	released, err := local.AdmissionStageReleased(reg)
	if err != nil || !released {
		t.Fatal("verified privacy receipt did not release stage", released, err)
	}
	committed, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := committed.CommittedSources()
	if err != nil || len(refs) != 1 || refs[0].SHA256 != pending.SourceSHA256 {
		t.Fatal(refs, err)
	}
	got, err := remote.Get(t.Context(), refs[0].Key)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := archive.ReadSourceBundle(bytes.NewReader(got), archive.DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range decoded.SupplementalEvidence {
		if e.Kind == archive.EvidenceKindSkillSnapshot {
			t.Fatal("obsolete staged skill body uploaded")
		}
	}
	if _, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || found {
		t.Fatal("covered stage request remains", found, err)
	}
}

func TestRetainedPrivacyProofRejectsIncompleteSwapAgeAndAuthority(t *testing.T) {
	for _, mode := range []string{"omit", "swap", "age", "authority", "policy"} {
		t.Run(mode, func(t *testing.T) {
			s, m, raw := retainedHistoryFixture(t)
			p, err := s.prepareRetainedPrivacy(raw, remoteRetainedLoader(s.remote, m), s.published.PublicationPredecessor(), state.PrivacyCommitted, "", "", "body")
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "omit":
				p.Commit.Privacy.Sources = p.Commit.Privacy.Sources[:1]
			case "swap":
				p.Commit.Privacy.Sources[1].Next = p.Commit.Privacy.Sources[0].Next
			case "age":
				p.Commit.Privacy.Sources[1].Previous.CapturedAt = p.Commit.Privacy.Sources[1].Previous.CapturedAt.Add(time.Minute)
			case "authority":
				p.Commit.Privacy.Authority = state.PrivacyStage
			case "policy":
				p.Commit.Privacy.Sources[1].NewPolicy.Skill = "body"
			}
			if err := p.ValidatePublication(); err == nil {
				t.Fatal("unbound privacy mutation accepted")
			}
		})
	}
}

func TestPendingPrivacyRetainsAuthorizedNewerCandidateWithoutUploadingObsoleteBytes(t *testing.T) {
	s, m, raw := retainedHistoryFixture(t)
	before, err := reader.LoadSource(t.Context(), s.remote, m, reader.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	candidate := before
	candidate.Capture.CapturedAt = before.Capture.CapturedAt.Add(time.Hour)
	candidate.NativeRecords = append(append([]map[string]any(nil), before.NativeRecords...), map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "newer authorized candidate"}})
	candidate.Ordinals = append(append([]uint64(nil), before.Ordinals...), 3)
	history := *before.History
	history.Spans = append([]archive.HistorySpan(nil), history.Spans...)
	history.Spans[0].EndOrdinal = 4
	history.Spans[0].EndRecord = 4
	candidate.History = &history
	compressed, err := archive.BuildCompressedSource(candidate)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.SourceObjectKey(candidate, compressed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	ref := archive.SourceReference{Key: key, SHA256: compressed.SHA256, CompressedBytes: len(compressed.Bytes)}
	next := m
	next.CapturedAt = candidate.Capture.CapturedAt
	next.SourceBundle = ref
	next.MetadataDerivedAt = s.now
	nextRaw, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	original := state.PendingPublication{Bundle: candidate, SourceKey: key, SourceSHA256: ref.SHA256, SourceBytes: compressed.Bytes, MetadataKey: s.mustMetadataKey(), MetadataBytes: nextRaw, SkillEvidence: "body", ReadyAt: s.now, Attempted: true}
	prior := s.published.PublicationPredecessor()
	adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
	if err != nil || !adapter.EvidenceExtends(before, candidate) {
		t.Fatal("original provider did not authorize continuation", err)
	}
	prior.SameRevisionContinuity = &state.PublicationContinuity{PreviousSourceSHA256: m.SourceBundle.SHA256, NextSourceSHA256: ref.SHA256}
	original, err = state.PreparePublication(original, prior, s.reg.DestinationID, s.publicationAdmission(), "original-filter-policy", state.PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.local.SavePending(s.id(), original); err != nil {
		t.Fatal(err)
	}
	authority, kind, err := s.reconcilePrivacyPending(original)
	if err != nil || kind != state.PrivacyPending {
		t.Fatal(kind, err)
	}
	loader, err := s.pendingRetainedLoader(original)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := s.prepareRetainedPrivacy(original.MetadataBytes, loader, authority, kind, "", "", original.SkillEvidence)
	if err != nil {
		t.Fatal(err)
	}
	if !replacement.Bundle.Capture.CapturedAt.Equal(candidate.Capture.CapturedAt) {
		t.Fatal("newer pending age lost")
	}
	encoded, err := json.Marshal(replacement.Bundle)
	if err != nil || !bytes.Contains(encoded, []byte("newer authorized candidate")) || bytes.Contains(encoded, []byte("obsolete-private-marker")) {
		t.Fatal("candidate was discarded or obsolete evidence retained", err, string(encoded))
	}
	if replacement.Commit.PredecessorSHA256 != storage.SHA256Hex(raw) || replacement.Commit.Privacy.Authority != state.PrivacyPending {
		t.Fatal("pending authority guessed", replacement.Commit)
	}
	if _, err := s.remote.Get(t.Context(), original.SourceKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("obsolete candidate was uploaded", err)
	}
}

func TestRetainedPrivacyRewritesFullReferenceLimitWithoutEviction(t *testing.T) {
	s, m, _ := retainedHistoryFixture(t)
	source, err := reader.LoadRevisionSource(t.Context(), s.remote, m, m.History.Preserved[0], reader.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	m.History.Preserved = nil
	for i := range archive.MaxPreservedRevisions {
		b := source
		h := *source.History
		h.Spans = append([]archive.HistorySpan(nil), source.History.Spans...)
		revision := fmt.Sprintf("%08x-1111-4111-8111-111111111111", i+10)
		h.ActiveRolloutID = revision
		h.Spans[0].RolloutID = revision
		b.History = &h
		b.Capture.CapturedAt = b.Capture.CapturedAt.Add(-time.Duration(i+1) * time.Hour)
		compressed, err := archive.BuildCompressedSource(b)
		if err != nil {
			t.Fatal(err)
		}
		key, err := archive.SourceObjectKey(b, compressed.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.remote.Put(t.Context(), key, compressed.Bytes); err != nil {
			t.Fatal(err)
		}
		m.History.Preserved = append(m.History.Preserved, archive.RevisionReference{RevisionID: revision, CapturedAt: b.Capture.CapturedAt, Source: archive.SourceReference{Key: key, SHA256: compressed.SHA256, CompressedBytes: len(compressed.Bytes)}})
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	current, _, _ := s.published.LastPublished()
	if err := s.published.SavePublication(current, s.now, m.SourceBundle, raw); err != nil {
		t.Fatal(err)
	}
	pending, err := s.prepareRetainedPrivacy(raw, remoteRetainedLoader(s.remote, m), s.published.PublicationPredecessor(), state.PrivacyCommitted, "", "", "body")
	if err != nil || len(pending.Sources) != archive.MaxPreservedRevisions+1 || len(pending.Commit.Privacy.Sources) != archive.MaxPreservedRevisions+1 {
		t.Fatal("capacity evicted retained evidence", len(pending.Sources), err)
	}
}
