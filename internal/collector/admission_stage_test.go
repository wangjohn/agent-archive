package collector

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stagedFixture(t *testing.T) (*state.Store, archive.SessionRegistration) {
	t.Helper()
	return stagedPolicyFixture(t, config.SkillEvidenceBody)
}

func stagedPolicyFixture(t *testing.T, mode config.SkillEvidence, extra ...archive.SupplementalEvidence) (*state.Store, archive.SessionRegistration) {
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

func TestAdmissionStageRetainedSkillEvidenceMustMatchCurrentPolicy(t *testing.T) {
	extra := archive.SupplementalEvidence{Kind: archive.EvidenceKindSkillSnapshot, Provenance: "synthetic", Payload: map[string]any{"name": "synthetic", "body": "synthetic skill body"}}
	local, reg := stagedPolicyFixture(t, config.SkillEvidenceNone, extra)
	result, err := Run(t.Context(), local, storagetest.NewMemoryStore(), Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic", SkillEvidence: config.SkillEvidenceNone})
	if err != nil || result.Errors[reg.ArchiveSessionID] == nil || !strings.Contains(result.Errors[reg.ArchiveSessionID].Error(), "privacy policy") || len(result.Published) != 0 {
		t.Fatal(result, err)
	}
	released, err := local.AdmissionStageReleased(reg)
	if err != nil || released {
		t.Fatal("privacy mismatch released evidence", released, err)
	}
}
