package collector

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"os"
	"path/filepath"
	"testing"
)

func stagedFixture(t *testing.T) (*state.Store, archive.SessionRegistration) {
	t.Helper()
	return stagedPolicyFixture(t, config.SkillEvidenceBody)
}
func stagedPolicyFixture(t *testing.T, mode config.SkillEvidence, extra ...archive.SupplementalEvidence) (*state.Store, archive.SessionRegistration) {
	t.Helper()
	return stagedPolicyFixtureWithReservation(t, mode, 0, extra...)
}
func stagedPolicyFixtureWithReservation(t *testing.T, mode config.SkillEvidence, reserved int64, extra ...archive.SupplementalEvidence) (*state.Store, archive.SessionRegistration) {
	t.Helper()
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "synthetic.jsonl", codexTranscript)
	reg := registration(t, path)
	reg.Origin = archive.SessionOriginImport
	reg.ImportBatch = archive.NewImportBatch("synthetic")
	reg.AdmittedAt = reg.RegisteredAt
	bundle, err := ReadLocalBundle(t.Context(), local.Home(), reg, reg.RegisteredAt, "", testSources)
	if err != nil {
		t.Fatal(err)
	}
	for i := range extra {
		if extra[i].ObservedAt.IsZero() {
			extra[i].ObservedAt = reg.RegisteredAt
		}
	}
	bundle.SupplementalEvidence = append(bundle.SupplementalEvidence, extra...)
	digest, err := local.PrepareAdmissionStage(reg, bundle, string(mode), reg.RegisteredAt)
	if err != nil {
		t.Fatal(err)
	}
	if reserved > 0 {
		manifest, _, err := local.ReadAdmissionStage(reg.ArchiveSessionID, digest)
		if err != nil {
			t.Fatal(err)
		}
		manifest.ReservedBytes = reserved
		encoded, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(local.Home(), "admission-stages", reg.ArchiveSessionID+".json"), encoded, 0600); err != nil {
			t.Fatal(err)
		}
		digest = storage.SHA256Hex(encoded)
	}
	reg.AdmissionStage = digest
	if err = local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err = local.SaveAdmissionRequest(reg); err != nil {
		t.Fatal(err)
	}
	return local, reg
}
func TestAdmissionStagePublishesAfterDeletionAndPreservesNewerRequest(t *testing.T) {
	local, reg := stagedFixture(t)
	before, _, err := local.LoadRequest(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = local.SaveRequest(reg.ArchiveSessionID, "stop", reg.RegisteredAt.Add(1000000000)); err != nil {
		t.Fatal(err)
	}
	newer, _, err := local.LoadRequest(reg.ArchiveSessionID)
	if err != nil || newer.Token == before.Token || newer.StageToken != before.Token {
		t.Fatal(newer, err)
	}
	if err = os.Remove(reg.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	result, err := Run(t.Context(), local, remote, Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic", RepoKey: func(string) string { t.Fatal("stage asked Git"); return "" }})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result, err)
	}
	pending, found, err := local.LoadRequest(reg.ArchiveSessionID)
	if err != nil || !found || pending.Token != newer.Token {
		t.Fatal("newer request acknowledged", pending, found, err)
	}
	released, err := local.AdmissionStageReleased(reg)
	if err != nil || !released {
		t.Fatal(released, err)
	}
}
func TestAdmissionStageCorruptionNeverFallsBackToNative(t *testing.T) {
	local, reg := stagedFixture(t)
	if err := os.WriteFile(filepath.Join(local.Home(), "admission-stages", reg.ArchiveSessionID+".source.gz"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := Run(t.Context(), local, storagetest.NewMemoryStore(), Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic"})
	if err != nil || !errors.Is(result.Errors[reg.ArchiveSessionID], state.ErrAdmissionStageRecovery) || len(result.Published) != 0 {
		t.Fatal(result, err)
	}
	owed, err := local.Outstanding(reg, true)
	if err != nil || !owed.Stage || !owed.DefersExpiry() {
		t.Fatal(owed, err)
	}
}

func TestAdmissionStagePrivacyMakesProgressAtSaturatedQuotaAfterNativeDeletion(t *testing.T) {
	extra := archive.SupplementalEvidence{Kind: archive.EvidenceKindSkillSnapshot, Provenance: "synthetic", Payload: map[string]any{"name": "synthetic", "body": "synthetic skill body"}}
	local, reg := stagedPolicyFixtureWithReservation(t, config.SkillEvidenceBody, state.AdmissionStageQuota, extra)
	manifest, _, err := local.ReadAdmissionStage(reg.ArchiveSessionID, reg.AdmissionStage)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", reg.RegisteredAt.Add(1000000000)); err != nil {
		t.Fatal(err)
	}
	newer, _, err := local.LoadRequest(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := state.NewTemporaryReservation(local, state.PublicationPrivacy, reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Reserve(1); !errors.Is(err, state.ErrAdmissionStageCapacity) {
		t.Fatal("fixture is not saturated", err)
	}
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(reg.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	result, err := Run(t.Context(), local, remote, Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic", SkillEvidence: config.SkillEvidenceNone, RepoKey: func(string) string { t.Fatal("refilter asked Git"); return "" }})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result, err)
	}
	metadata := fetchMetadata(t, remote, reg.Harness.Name, reg.ArchiveSessionID)
	source, err := remote.Get(t.Context(), metadata.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := archive.ReadSourceBundle(bytes.NewReader(source), archive.DecodeOptions{})
	if err != nil || !sourceEvidenceWithinPolicy(bundle.SupplementalEvidence, config.SkillEvidenceNone) {
		t.Fatal("obsolete bytes published", bundle, err)
	}
	if !bundle.Capture.CapturedAt.Equal(manifest.PreparedAt) {
		t.Fatal("capture age changed", bundle.Capture.CapturedAt, manifest.PreparedAt)
	}
	released, err := local.AdmissionStageReleased(reg)
	if err != nil || !released {
		t.Fatal("verified transform did not release immutable stage", released, err)
	}
	queued, found, err := local.LoadRequest(reg.ArchiveSessionID)
	if err != nil || !found || queued.Token != newer.Token {
		t.Fatal("newer request lost", queued, found, err)
	}
	if _, found, err := local.LoadPending(reg.ArchiveSessionID); err != nil || found {
		t.Fatal("committed pending retained", found, err)
	}
	reservation, err = state.NewTemporaryReservation(local, state.PublicationPrivacy, reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Reserve(1); err != nil {
		t.Fatal("stage release did not restore capacity", err)
	}
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionStageComposesPrivacyReceiptAcrossCommitCrash(t *testing.T) {
	extra := archive.SupplementalEvidence{Kind: archive.EvidenceKindSkillSnapshot, Provenance: "synthetic", Payload: map[string]any{"name": "synthetic", "body": "synthetic private body"}}
	inventory := archive.SupplementalEvidence{Kind: archive.EvidenceKindSkillInventory, Provenance: "synthetic", Payload: map[string]any{"name": "synthetic"}}
	local, reg := stagedPolicyFixture(t, config.SkillEvidenceBody, extra, inventory)
	if err := os.Remove(reg.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic", SkillEvidence: config.SkillEvidenceMetadata}
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] == nil {
		t.Fatal("expected upload checkpoint", result, err)
	}
	first, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found || first.Commit == nil || first.Commit.Privacy == nil {
		t.Fatal(first, found, err)
	}
	remote.failMetadata = false
	published, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	scan := newSessionScan(t.Context(), local, remote, reg, state.Request{}, published, reg.RegisteredAt, opts)
	if err := scan.upload(first); err != nil {
		t.Fatal(err)
	}
	if err := published.SaveCommittedPublication(first, reg.RegisteredAt); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after exact remote/local commit, before covered request and release.
	if released, err := local.AdmissionStageReleased(reg); err != nil || released {
		t.Fatal("stage prematurely released", released, err)
	}
	opts.SkillEvidence = config.SkillEvidenceNone
	result, err = Run(t.Context(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result, err)
	}
	published, err = local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	prior := published.PublicationPredecessor()
	proof := prior.RetainedPrivacy
	if proof == nil || proof.StageOrigin == nil || proof.StageOrigin.Previous.Source.SHA256 != first.Commit.Privacy.StageSourceSHA256 || proof.StageOrigin.Next.Source.SHA256 == first.SourceSHA256 || len(proof.Sources) != 1 || proof.StagePriorMetadataSHA256 != first.Commit.MetadataSHA256 {
		t.Fatal("bounded composed receipt lost original stage", proof)
	}
	if released, err := local.AdmissionStageReleased(reg); err != nil || !released {
		t.Fatal("composed receipt failed cleanup", released, err)
	}
	metadata := fetchMetadata(t, remote, reg.Harness.Name, reg.ArchiveSessionID)
	raw, err := remote.Get(t.Context(), metadata.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := archive.ReadSourceBundle(bytes.NewReader(raw), archive.DecodeOptions{})
	if err != nil || !sourceEvidenceWithinPolicy(bundle.SupplementalEvidence, config.SkillEvidenceNone) {
		t.Fatal("obsolete evidence selected", bundle, err)
	}
}
