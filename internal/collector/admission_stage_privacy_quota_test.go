package collector

import (
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestAdmissionStageRepeatedPrivacyChangesProgressAtSaturatedQuota(t *testing.T) {
	t.Parallel()
	// Saturate the composed pool, including its fixed shared cleanup allowance.
	local, reg := stagedPolicyFixtureWithReservation(t, config.SkillEvidenceBody, state.AdmissionStageQuota-(64<<10),
		archive.SupplementalEvidence{Kind: archive.EvidenceKindSkillSnapshot, Provenance: "synthetic", Payload: map[string]any{"name": "synthetic", "snapshot": "private-stage-original"}})
	manifest, _, err := local.ReadAdmissionStage(reg.ArchiveSessionID, reg.AdmissionStage)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(reg.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic"}
	for i, mode := range []config.SkillEvidence{config.SkillEvidenceNone, config.SkillEvidenceBody, config.SkillEvidenceNone, config.SkillEvidenceBody} {
		opts.SkillEvidence = mode
		at := reg.RegisteredAt.Add(time.Hour + time.Duration(i)*time.Minute)
		opts.Now = func() time.Time { return at }
		result, err := Run(t.Context(), local, remote, opts)
		if err != nil || result.Errors[reg.ArchiveSessionID] == nil {
			t.Fatal("expected uncommitted metadata checkpoint", result, err)
		}
		pending, found, err := local.LoadPending(reg.ArchiveSessionID)
		if err != nil || !found || pending.SkillEvidence != string(mode) {
			t.Fatal("saturated privacy transition stalled", i, pending.SkillEvidence, found, err, result.Errors)
		}
		if i == 0 && pending.SourceSHA256 == manifest.SHA256 {
			t.Fatal("first reduction did not transform the frozen source")
		}
		if !sourceEvidenceWithinPolicy(pending.Bundle.SupplementalEvidence, mode) || (len(pending.Bundle.SupplementalEvidence) == 0) != (mode == config.SkillEvidenceNone) {
			t.Fatal("current policy did not refilter immutable original content", i)
		}
		if released, err := local.AdmissionStageReleased(reg); err != nil || released {
			t.Fatal("uncommitted privacy released stage", released, err)
		}
		if _, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || !found {
			t.Fatal("uncommitted privacy acknowledged request", found, err)
		}
		local, err = state.Open(local.Home())
		if err != nil {
			t.Fatal(err)
		}
	}
	remote.failMetadata = false
	if result, err := Run(t.Context(), local, remote, opts); err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal("saturated privacy successor did not settle", result, err)
	}
	if released, err := local.AdmissionStageReleased(reg); err != nil || !released {
		t.Fatal("verified successor did not release stage", released, err)
	}
}
