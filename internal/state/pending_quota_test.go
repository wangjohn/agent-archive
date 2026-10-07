package state

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

func saturatedStagePending(t *testing.T) (*Store, archive.SessionRegistration, PendingPublication) {
	t.Helper()
	s, reg, bundle := stageFixture(t)
	digest, err := s.PrepareAdmissionStage(reg, bundle, "none", reg.AdmittedAt)
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := s.ReadAdmissionStage(reg.ArchiveSessionID, digest)
	if err != nil {
		t.Fatal(err)
	}
	m.ReservedBytes = AdmissionStageQuota
	encoded, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := s.stagePath(reg.ArchiveSessionID, ".json")
	if err = local.WriteBytes(manifest, encoded); err != nil {
		t.Fatal(err)
	}
	reg.AdmissionStage = stageDigest(encoded)
	if err = s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	object, _ := s.stagePath(reg.ArchiveSessionID, ".source.gz")
	compressed, err := os.ReadFile(object)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.SourceObjectKey(bundle, m.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	ref := archive.SourceReference{Key: key, SHA256: m.SHA256, CompressedBytes: len(compressed)}
	metadata, err := archive.BuildMetadataWithAnalysis(bundle, archive.Analysis{}, nil, "synthetic", reg.SessionStartedAt, reg.AdmittedAt, ref, archive.ParserInfo{Name: "claude-code", Version: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	mk, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	p, err := PreparePublication(PendingPublication{AdmissionStage: reg.AdmissionStage, Bundle: bundle, SourceKey: key, MetadataKey: mk, SourceSHA256: m.SHA256, SourceBytes: compressed, MetadataBytes: raw, ReadyAt: reg.AdmittedAt}, PublicationPredecessor{State: PredecessorAbsent}, reg.DestinationID, AdmissionStageContext(reg), "synthetic-policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	return s, reg, p
}

func TestPendingCoveredStageSaturatedQuotaMakesProgress(t *testing.T) {
	t.Parallel()
	s, reg, p := saturatedStagePending(t)
	if used, err := s.admissionStageUsage(); err != nil || used != AdmissionStageQuota {
		t.Fatal(used, err)
	}
	if err := s.SavePending(reg.ArchiveSessionID, p); err != nil {
		t.Fatal("held future allowance did not permit publication", err)
	}
	p.Attempted = true
	if err := s.SavePending(reg.ArchiveSessionID, p); err != nil {
		t.Fatal("old/new replacement deadlocked", err)
	}
	if used, err := s.admissionStageUsage(); err != nil || used != AdmissionStageQuota {
		t.Fatal("undercharged held stage", used, err)
	}
	m, _, err := s.ReadAdmissionStage(reg.ArchiveSessionID, reg.AdmissionStage)
	if err != nil {
		t.Fatal(err)
	}
	published, err := s.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SaveCommittedPublication(p, reg.AdmittedAt); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseAdmissionStage(reg, m, published, ""); err != nil {
		t.Fatal(err)
	}
	if used, err := s.admissionStageUsage(); err != nil || used >= AdmissionStageQuota {
		t.Fatal("release made no progress", used, err)
	}
}

func TestPendingCoveredAllowanceRejectsTamperedProof(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"digest", "source", "bundle"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			s, reg, p := saturatedStagePending(t)
			switch kind {
			case "digest":
				p.AdmissionStage = "invalid"
			case "source":
				p.SourceBytes = []byte("different")
			case "bundle":
				p.Bundle.NativeSessionID = "different"
			}
			p.Commit = nil
			if err := s.SavePending(reg.ArchiveSessionID, p); !errors.Is(err, ErrAdmissionStageCapacity) {
				t.Fatal("borrowed malformed proof", err)
			}
			if _, err := os.Stat(s.pendingPath(reg.ArchiveSessionID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("allocated before quota", err)
			}
		})
	}
}

func TestPendingRAMOnlyPrivacyHandleUsesHeldStageAllowance(t *testing.T) {
	t.Parallel()
	s, reg, p := saturatedStagePending(t)
	handle, err := NewTemporaryReservation(s, PublicationPrivacy, reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SavePendingWithTemporaryReservation(handle, reg.ArchiveSessionID, p); err != nil {
		t.Fatal("zero scratch consumer deadlocked", err)
	}
	if err = handle.Close(); err != nil {
		t.Fatal(err)
	}
	if err = handle.Close(); err != nil {
		t.Fatal("zero close is not idempotent", err)
	}
	if _, err = os.Stat(handle.Root()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("RAM consumer allocated scratch", err)
	}
}

func TestPendingRAMOnlyPrivacyHandleRejectsInvalidOwnership(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"owner", "key", "root", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			s, reg, p := saturatedStagePending(t)
			owner, key := PublicationPrivacy, reg.ArchiveSessionID
			if kind == "owner" {
				owner = CursorAdmission
			}
			if kind == "key" {
				key = "different"
			}
			handle, err := NewTemporaryReservation(s, owner, key)
			if err != nil {
				t.Fatal(err)
			}
			defer handle.Close()
			if kind == "root" {
				if err = os.MkdirAll(handle.Root(), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "corrupt" {
				handle.err = ErrAdmissionStageRecovery
			}
			if err = s.SavePendingWithTemporaryReservation(handle, reg.ArchiveSessionID, p); !errors.Is(err, ErrAdmissionStageRecovery) {
				t.Fatal("invalid handle permitted write", err)
			}
			if _, err = os.Stat(s.pendingPath(reg.ArchiveSessionID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid handle allocated pending", err)
			}
		})
	}
}

func TestPendingQuotaCorruptStageCannotUndercharge(t *testing.T) {
	t.Parallel()
	s, reg, _ := saturatedStagePending(t)
	manifest, _ := s.stagePath(reg.ArchiveSessionID, ".json")
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var m AdmissionStage
	if err = json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m.ReservedBytes = 1
	if err = local.Write(manifest, m); err != nil {
		t.Fatal(err)
	}
	if _, err = s.admissionStageUsage(); !errors.Is(err, ErrAdmissionStageRecovery) {
		t.Fatal("undercharged corrupted reservation", err)
	}
}
