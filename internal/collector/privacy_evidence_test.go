package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type privacyEvidenceFailure string

const (
	privacyEvidenceFailureUnreadableSource privacyEvidenceFailure = "unreadable-source"
	privacyEvidenceFailureMissingBody      privacyEvidenceFailure = "missing-body"
	privacyEvidenceFailureChangedBody      privacyEvidenceFailure = "changed-body"
)

func unuploadedPrivacyFixture(t *testing.T) (*state.Store, archive.SessionRegistration, *metadataFailStore, Options, state.PendingPublication) {
	t.Helper()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "synthetic.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", reg.RegisteredAt); err != nil {
		t.Fatal(err)
	}
	remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic", SkillEvidence: config.SkillEvidenceBody, Now: func() time.Time { return reg.RegisteredAt.Add(time.Hour) }, RepoKey: func(string) string { return "synthetic" }, SupplementalEvidence: func(_ archive.SessionRegistration, at time.Time) ([]archive.SupplementalEvidence, error) {
		return []archive.SupplementalEvidence{{Kind: archive.EvidenceKindSkillSnapshot, Provenance: "synthetic", ObservedAt: at, Payload: map[string]any{"name": "synthetic", "snapshot": "private-original-evidence"}}}, nil
	}}
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] == nil {
		t.Fatal("expected initial uncertain metadata", result, err)
	}
	old, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found || old.Commit == nil {
		t.Fatal(old, found, err)
	}
	if err := remote.Delete(t.Context(), old.SourceKey); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(reg.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	opts.SkillEvidence = config.SkillEvidenceNone
	opts.SupplementalEvidence = func(archive.SessionRegistration, time.Time) ([]archive.SupplementalEvidence, error) {
		t.Fatal("privacy replay observed native skill inventory")
		return nil, nil
	}
	return local, reg, remote, opts, old
}

func TestPrivacyOriginalJournalSurvivesRestartUntilLocalSuccessor(t *testing.T) {
	local, reg, remote, opts, old := unuploadedPrivacyFixture(t)
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] == nil {
		t.Fatal("expected successor upload checkpoint", result, err)
	}
	next, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found || next.Commit.Privacy.InputJournalSHA256 == "" || next.SourceSHA256 == old.SourceSHA256 {
		t.Fatal(next, found, err)
	}
	raw, err := local.ReadPublicationEvidence(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	var journal privacyInputJournal
	if err := json.Unmarshal(raw, &journal); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(journal.Original.SourceBytes, old.SourceBytes) || !bytes.Equal(journal.Original.MetadataBytes, old.MetadataBytes) || journal.ReplacementMetadataSHA256 != next.Commit.MetadataSHA256 {
		t.Fatal("original bytes were replaced before local commit")
	}
	if _, err := remote.Get(t.Context(), old.SourceKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("obsolete original reuploaded", err)
	}
	local, err = state.Open(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	remote.failMetadata = false
	result, err = Run(t.Context(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result, err)
	}
	if _, err := local.ReadPublicationEvidence(reg.ArchiveSessionID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("original evidence not retired after exact local commit", err)
	}
	metadata := fetchMetadata(t, remote, reg.Harness.Name, reg.ArchiveSessionID)
	source, err := remote.Get(t.Context(), metadata.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := archive.ReadSourceBundle(bytes.NewReader(source), archive.DecodeOptions{})
	if err != nil || !sourceEvidenceWithinPolicy(bundle.SupplementalEvidence, config.SkillEvidenceNone) {
		t.Fatal("obsolete evidence selected", bundle, err)
	}
}

func TestPrivacyOriginalJournalMissingOrCorruptKeepsObligation(t *testing.T) {
	for _, kind := range []string{"missing", "corrupt"} {
		t.Run(string(kind), func(t *testing.T) {
			local, reg, remote, opts, _ := unuploadedPrivacyFixture(t)
			_, _ = Run(t.Context(), local, remote, opts)
			before, found, err := local.LoadPending(reg.ArchiveSessionID)
			if err != nil || !found || before.Commit.Privacy.InputJournalSHA256 == "" {
				t.Fatal(before, found, err)
			}
			if kind == "missing" {
				if err := local.RemovePublicationEvidence(reg.ArchiveSessionID); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(filepath.Join(local.Home(), "publication-evidence", reg.ArchiveSessionID, "journal.json"), []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			remote.failMetadata = false
			result, err := Run(t.Context(), local, remote, opts)
			if err != nil || result.Errors[reg.ArchiveSessionID] == nil || len(result.Published) != 0 {
				t.Fatal("missing original evidence bypassed", result, err)
			}
			after, found, err := local.LoadPending(reg.ArchiveSessionID)
			if err != nil || !found || after.Commit.MetadataSHA256 != before.Commit.MetadataSHA256 {
				t.Fatal("pending evidence lost", after, found, err)
			}
			if _, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || !found {
				t.Fatal("uncommitted request completed", found, err)
			}
		})
	}
}

func TestPrivacyOriginalJournalSurvivesFailedLocalCommit(t *testing.T) {
	local, reg, remote, opts, old := unuploadedPrivacyFixture(t)
	published, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(local.Home(), "published", reg.ArchiveSessionID+".json")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	remote.failMetadata = false
	scan := newSessionScan(t.Context(), local, remote, reg, state.Request{}, published, opts.Now(), opts)
	if _, err := scan.maintainPendingPrivacy(old); err == nil {
		t.Fatal("synthetic local commit failure was ignored")
	}
	next, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found || next.Commit.Privacy.InputJournalSHA256 == "" {
		t.Fatal(next, found, err)
	}
	body, err := remote.Get(t.Context(), next.MetadataKey)
	if err != nil || storage.SHA256Hex(body) != next.Commit.MetadataSHA256 {
		t.Fatal("failed before intended remote commit", err)
	}
	if _, err := local.ReadPublicationEvidence(reg.ArchiveSessionID); err != nil {
		t.Fatal("remote PUT released original evidence", err)
	}
	if _, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || !found {
		t.Fatal("failed local save acknowledged request", found, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result, err)
	}
	if _, err := local.ReadPublicationEvidence(reg.ArchiveSessionID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("original evidence not cleaned after exact local completion", err)
	}
}

func TestPrivacyEmbeddedRemoteSourceLossKeepsPending(t *testing.T) {
	local, reg, remote, opts, old := unuploadedPrivacyFixture(t)
	// Restore exact immutable original remotely: the replacement may reference it
	// rather than duplicating bytes in a charged journal.
	if err := remote.Put(t.Context(), old.SourceKey, old.SourceBytes); err != nil {
		t.Fatal(err)
	}
	_, _ = Run(t.Context(), local, remote, opts)
	next, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found || next.Commit.Privacy.InputJournalSHA256 != "" || next.Commit.Privacy.ReplayInput == nil {
		t.Fatal(next, found, err)
	}
	if err := remote.Delete(t.Context(), old.SourceKey); err != nil {
		t.Fatal(err)
	}
	remote.failMetadata = false
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] == nil || len(result.Published) != 0 {
		t.Fatal("vanished original backing accepted", result, err)
	}
	after, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found || after.Commit.MetadataSHA256 != next.Commit.MetadataSHA256 {
		t.Fatal("source loss discarded pending", after, found, err)
	}
	if _, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || !found {
		t.Fatal("source loss completed request", found, err)
	}
}

type unreadablePrivacySourceStore struct {
	storage.ObjectStore
	key string
}

func (s unreadablePrivacySourceStore) Get(ctx context.Context, key string) ([]byte, error) {
	if key == s.key {
		return nil, os.ErrPermission
	}
	return s.ObjectStore.Get(ctx, key)
}

func TestPrivacyEmbeddedOriginalAuthorityMustRemainReadableAndSealed(t *testing.T) {
	for _, kind := range []privacyEvidenceFailure{privacyEvidenceFailureUnreadableSource, privacyEvidenceFailureMissingBody, privacyEvidenceFailureChangedBody} {
		t.Run(string(kind), func(t *testing.T) {
			local, reg, remote, opts, old := unuploadedPrivacyFixture(t)
			if err := remote.Put(t.Context(), old.SourceKey, old.SourceBytes); err != nil {
				t.Fatal(err)
			}
			_, _ = Run(t.Context(), local, remote, opts)
			next, found, err := local.LoadPending(reg.ArchiveSessionID)
			if err != nil || !found || next.Commit.Privacy.ReplayInput == nil || next.Commit.Privacy.InputJournalSHA256 != "" {
				t.Fatal(next, found, err)
			}
			var target storage.ObjectStore = remote
			switch kind {
			case privacyEvidenceFailureUnreadableSource:
				target = unreadablePrivacySourceStore{ObjectStore: remote, key: old.SourceKey}
			case privacyEvidenceFailureMissingBody:
				next.Commit.Privacy.ReplayInput.MetadataBytes = nil
			case privacyEvidenceFailureChangedBody:
				next.Commit.Privacy.ReplayInput.MetadataBytes = []byte("{}")
			}
			remote.failMetadata = false
			published, err := local.LoadPublishedState(reg.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			scan := newSessionScan(t.Context(), local, target, reg, state.Request{}, published, opts.Now(), opts)
			if _, err := scan.publishPending(next); err == nil {
				t.Fatal("unreadable or unsealed original authorized publication")
			}
			if _, err := remote.Get(t.Context(), next.MetadataKey); !errors.Is(err, storage.ErrNotFound) {
				t.Fatal("invalid original allowed metadata upload", err)
			}
			after, found, err := local.LoadPending(reg.ArchiveSessionID)
			if err != nil || !found || after.Commit.MetadataSHA256 != next.Commit.MetadataSHA256 {
				t.Fatal("invalid original discarded pending", after, found, err)
			}
			if _, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || !found {
				t.Fatal("invalid original acknowledged request", found, err)
			}
		})
	}
}

func TestPrivacyOrphanEvidenceKeepsWorkVisibleWithoutNativeFallback(t *testing.T) {
	local, reg, remote, opts, _ := unuploadedPrivacyFixture(t)
	_, _ = Run(t.Context(), local, remote, opts)
	if err := local.RemovePending(reg.ArchiveSessionID); err != nil {
		t.Fatal(err)
	}
	owed, err := local.Outstanding(reg, false)
	if err != nil || !owed.Upload || !owed.Pending() || !owed.DefersExpiry() {
		t.Fatal("original evidence lost work obligation", owed, err)
	}
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] == nil || len(result.Published) != 0 {
		t.Fatal("orphan original evidence reopened native", result, err)
	}
	if _, err := local.ReadPublicationEvidence(reg.ArchiveSessionID); err != nil {
		t.Fatal("orphan evidence dropped", err)
	}
}

func TestPrivacyPreparedEvidenceQuotaFailureKeepsOriginalPending(t *testing.T) {
	local, reg, remote, opts, old := unuploadedPrivacyFixture(t)
	published, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	scan := newSessionScan(t.Context(), local, remote, reg, state.Request{}, published, opts.Now(), opts)
	prior, kind, err := scan.reconcilePrivacyPending(old)
	if err != nil {
		t.Fatal(err)
	}
	loader, err := scan.pendingRetainedLoader(old)
	if err != nil {
		t.Fatal(err)
	}
	next, err := scan.prepareRetainedPrivacy(old.MetadataBytes, loader, prior, kind, "", "", old.SkillEvidence)
	if err != nil {
		t.Fatal(err)
	}
	next.RequestToken = old.RequestToken
	if err := scan.preservePrivacyInput(old, &next); err != nil {
		t.Fatal(err)
	}
	filler := filepath.Join(local.Home(), "pending", "synthetic-capacity.json")
	f, err := os.Create(filler)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(state.AdmissionStageQuota / 2); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := scan.persistPrivacy(next); !errors.Is(err, state.ErrAdmissionStageCapacity) {
		t.Fatal("expected aggregate quota stop", err)
	}
	current, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found || !bytes.Equal(current.SourceBytes, old.SourceBytes) || current.Commit.MetadataSHA256 != old.Commit.MetadataSHA256 {
		t.Fatal("failed replacement discarded original pending", current, found, err)
	}
	if _, err := local.ReadPublicationEvidence(reg.ArchiveSessionID); err != nil {
		t.Fatal("prepared evidence disappeared", err)
	}
	if err := os.Remove(filler); err != nil {
		t.Fatal(err)
	}
	remote.failMetadata = false
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal("prepared evidence did not reconcile", result, err)
	}
}

func TestPrivacyRepeatedUncommittedPolicyChangesPreserveOldestOriginal(t *testing.T) {
	for _, remoteOriginal := range []bool{false, true} {
		t.Run(fmt.Sprintf("remote-original-%t", remoteOriginal), func(t *testing.T) {
			local, reg, remote, opts, old := unuploadedPrivacyFixture(t)
			if remoteOriginal {
				if err := remote.Put(t.Context(), old.SourceKey, old.SourceBytes); err != nil {
					t.Fatal(err)
				}
			}
			for i := range 5 {
				at := reg.RegisteredAt.Add(time.Hour + time.Duration(i+1)*time.Minute)
				opts.Now = func() time.Time { return at }
				if i%2 == 0 {
					opts.SkillEvidence = config.SkillEvidenceNone
				} else {
					opts.SkillEvidence = config.SkillEvidenceMetadata
				}
				result, err := Run(t.Context(), local, remote, opts)
				if err != nil || result.Errors[reg.ArchiveSessionID] == nil {
					t.Fatal("expected remote metadata checkpoint", result, err)
				}
				next, found, err := local.LoadPending(reg.ArchiveSessionID)
				if err != nil || !found || next.Commit.Privacy == nil || next.SkillEvidence != string(opts.SkillEvidence) {
					t.Fatal("repeated uncommitted transition stalled", next.SkillEvidence, found, err, result.Errors)
				}
				root := next.Commit.Privacy.ReplayInput
				if root == nil || i < 3 && (root.MetadataSHA256 != old.Commit.MetadataSHA256 || root.SourceSetSHA256 != old.Commit.SourceSetSHA256) || root.Privacy != nil && (root.Privacy.ReplayInput != nil || root.Privacy.PendingMutation != nil && root.Privacy.PendingMutation.Privacy != nil) {
					t.Fatal("oldest authority changed or retained a receipt chain", root)
				}
				if i == 2 {
					published, err := local.LoadPublishedState(reg.ArchiveSessionID)
					if err != nil {
						t.Fatal(err)
					}
					remote.failMetadata = false
					scan := newSessionScan(t.Context(), local, remote, reg, state.Request{}, published, reg.RegisteredAt, opts)
					if err := scan.upload(next); err != nil {
						t.Fatal(err)
					}
					// The next policy pass must record exact remote-next without acknowledging
					// or releasing the original obligation before its selecting successor.
					remote.failMetadata = true
				}
				if !remoteOriginal {
					raw, err := local.ReadPublicationEvidence(reg.ArchiveSessionID)
					if err != nil {
						t.Fatal(err)
					}
					var j privacyInputJournal
					if err := json.Unmarshal(raw, &j); err != nil || !bytes.Equal(j.Original.SourceBytes, old.SourceBytes) {
						t.Fatal("oldest bytes changed", err)
					}
				}
			}
			remote.failMetadata = false
			result, err := Run(t.Context(), local, remote, opts)
			if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
				t.Fatal(result, err)
			}
			if _, found, err := local.LoadPending(reg.ArchiveSessionID); err != nil || found {
				t.Fatal("successor not settled", found, err)
			}
			if _, err := local.ReadPublicationEvidence(reg.ArchiveSessionID); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("oldest original not cleaned", err)
			}
		})
	}
}

func TestStagedOrphanPrivacyEvidenceBlocksReplacement(t *testing.T) {
	local, reg := stagedPolicyFixture(t, config.SkillEvidenceBody,
		archive.SupplementalEvidence{Kind: archive.EvidenceKindSkillSnapshot, Provenance: "synthetic", Payload: map[string]any{"name": "synthetic", "body": "private original"}})
	if err := local.SavePublicationEvidence(reg.ArchiveSessionID, []byte("orphan original evidence")); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic", SkillEvidence: config.SkillEvidenceNone}
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] == nil || len(result.Published) != 0 {
		t.Fatal("stage bypassed orphan obligation", result, err)
	}
	if _, found, err := local.LoadPending(reg.ArchiveSessionID); err != nil || found {
		t.Fatal("orphan obligation replaced", found, err)
	}
	if _, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || !found {
		t.Fatal("orphan obligation acknowledged", found, err)
	}
	if released, err := local.AdmissionStageReleased(reg); err != nil || released {
		t.Fatal("orphan obligation released stage", released, err)
	}
}

func TestFrozenOrphanPrivacyEvidenceBlocksAcknowledgement(t *testing.T) {
	local, remote := newTestStore(t), storagetest.NewMemoryStore()
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	dir := publishCodexSession(t, local, remote, at)
	writeTranscript(t, dir, "codex.jsonl", truncatedCodexTranscript)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic", Now: func() time.Time { return at.Add(time.Hour) }, RepoKey: func(string) string { return "" }}
	if result, err := Run(t.Context(), local, remote, opts); err != nil || len(result.Errors) != 0 {
		t.Fatal(result, err)
	}
	reg, _, err := local.LoadRegistration("session-1")
	if err != nil {
		t.Fatal(err)
	}
	builder, err := PrepareGenerationRecovery(t.Context(), reg, at.Add(2*time.Hour), opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.BeginGenerationRecovery(reg.ArchiveSessionID, at.Add(2*time.Hour), builder); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", at.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := local.SavePublicationEvidence(reg.ArchiveSessionID, []byte("orphan original evidence")); err != nil {
		t.Fatal(err)
	}
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] == nil {
		t.Fatal("frozen maintenance bypassed orphan obligation", result, err)
	}
	if _, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || !found {
		t.Fatal("orphan obligation acknowledged", found, err)
	}
}

func TestStagedCurrentSkillPolicyStillRefiltersInconsistentEvidence(t *testing.T) {
	local, reg := stagedPolicyFixture(t, config.SkillEvidenceNone, archive.SupplementalEvidence{Kind: archive.EvidenceKindSkillSnapshot, Provenance: "synthetic", Payload: map[string]any{"name": "synthetic", "body": "private original"}})
	remote := storagetest.NewMemoryStore()
	result, err := Run(t.Context(), local, remote, Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic", SkillEvidence: config.SkillEvidenceNone})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result, err)
	}
	metadata := fetchMetadata(t, remote, reg.Harness.Name, reg.ArchiveSessionID)
	bundle := fetchBundle(t, remote, metadata)
	if !sourceEvidenceWithinPolicy(bundle.SupplementalEvidence, config.SkillEvidenceNone) {
		t.Fatal("current-policy label bypassed retained-content privacy")
	}
}

func TestPrivacyPendingReceiptRejectsDifferentImmediateInputReference(t *testing.T) {
	local, reg, remote, opts, _ := unuploadedPrivacyFixture(t)
	_, _ = Run(t.Context(), local, remote, opts)
	p, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found || p.Commit.Privacy.Authority != state.PrivacyPending {
		t.Fatal(p, found, err)
	}
	// Keep the sealed original body/digests, age, ownership and next selection,
	// but substitute a valid same-namespace source in its previous correspondence.
	p.Commit.Privacy.Sources[0].Previous.Source = p.SourceReference()
	if err := p.ValidatePublication(); err == nil {
		t.Fatal("receipt accepted a source absent from its sealed immediate input")
	}
}

func TestPrivacyPolicyChangeAfterCompletedStageReleaseUsesCommittedSource(t *testing.T) {
	local, reg := stagedPolicyFixture(t, config.SkillEvidenceBody, archive.SupplementalEvidence{Kind: archive.EvidenceKindSkillSnapshot, Provenance: "synthetic", Payload: map[string]any{"name": "synthetic", "body": "private original"}})
	remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic", SkillEvidence: config.SkillEvidenceBody}
	_, _ = Run(t.Context(), local, remote, opts)
	original, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	published, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	remote.failMetadata = false
	scan := newSessionScan(t.Context(), local, remote, reg, state.Request{}, published, reg.RegisteredAt, opts)
	if err := scan.upload(original); err != nil {
		t.Fatal(err)
	}
	if err := published.SaveCommittedPublication(original, reg.RegisteredAt); err != nil {
		t.Fatal(err)
	}
	manifest, _, err := local.ReadAdmissionStage(reg.ArchiveSessionID, reg.AdmissionStage)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.ReleaseAdmissionStage(reg, manifest, published, original.RequestToken); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(reg.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	// Pending removal was interrupted, after selecting local commit and release.
	opts.SkillEvidence = config.SkillEvidenceNone
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result, err)
	}
	bundle := fetchBundle(t, remote, fetchMetadata(t, remote, reg.Harness.Name, reg.ArchiveSessionID))
	if !sourceEvidenceWithinPolicy(bundle.SupplementalEvidence, config.SkillEvidenceNone) {
		t.Fatal("released original replayed obsolete skill policy")
	}
}

func TestPrivacyReplayAuthorizationBindsExactImmediateReceipt(t *testing.T) {
	local, reg, remote, opts, _ := unuploadedPrivacyFixture(t)
	_, _ = Run(t.Context(), local, remote, opts)
	original, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	published, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	scan := newSessionScan(t.Context(), local, remote, reg, state.Request{}, published, opts.Now(), opts)
	prior, kind, err := scan.reconcilePrivacyPending(original)
	if err != nil || kind != state.PrivacyPending {
		t.Fatal(kind, err)
	}
	root, err := scan.oldestPrivacyInput(original)
	if err != nil {
		t.Fatal(err)
	}
	prior, err = prior.CheckPrivacyReplayInput(original, *root, reg.DestinationID, scan.publicationAdmission())
	if err != nil {
		t.Fatal(err)
	}
	// An authorization checked before refiltering cannot be reused after changing
	// an otherwise structurally valid receipt's native codec provenance.
	original.Commit.Privacy.Sources[0].OldPolicy.Format = "different-native-format"
	loader, err := scan.pendingRetainedLoader(original)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scan.prepareRetainedPrivacy(original.MetadataBytes, loader, prior, kind, "", "", original.SkillEvidence); err == nil {
		t.Fatal("authorization reused for a changed immediate receipt")
	}
}
