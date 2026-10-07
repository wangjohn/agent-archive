package state

import (
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type stageAdapter struct{}

func (stageAdapter) Name() string { return "claude-code" }

func (stageAdapter) Version() string { return "synthetic" }

func stageFixture(t *testing.T) (*Store, archive.SessionRegistration, archive.SourceBundle) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	reg := archive.SessionRegistration{ArchiveSessionID: "synthetic", NativeSessionID: "native", Harness: archive.Harness{Name: "claude-code"}, ProjectRoot: "/synthetic", ProjectID: archive.ProjectID("/synthetic"), SessionStartedAt: at, RegisteredAt: at, AdmittedAt: at, Origin: archive.SessionOriginImport, ImportBatch: archive.NewImportBatch("synthetic")}
	b, err := archive.NewSourceBundle(reg, stageAdapter{}, archive.FilteredTranscript{Format: "jsonl", Records: [][]byte{[]byte(`{"type":"user","message":{"role":"user","content":"safe"}}`)}}, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, reg, b
}

func TestAdmissionStageCorruptionAndPreparedManifestNeverAdmit(t *testing.T) {
	s, reg, b := stageFixture(t)
	digest, err := s.PrepareAdmissionStage(reg, b, "none", reg.AdmittedAt)
	if err != nil {
		t.Fatal(err)
	}
	if regs, e := s.LoadRegistrations(); e != nil || len(regs) != 0 {
		t.Fatal("prepared manifest admitted", regs, e)
	}
	m, got, e := s.ReadAdmissionStage(reg.ArchiveSessionID, digest)
	if e != nil || got.NativeSessionID != reg.NativeSessionID || m.ReservedBytes <= m.Bytes {
		t.Fatal(m, got, e)
	}
	info, e := os.Stat(filepath.Join(s.Home(), admissionStageDir, reg.ArchiveSessionID+".source.gz"))
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, e)
	}
	if e = os.WriteFile(filepath.Join(s.Home(), admissionStageDir, reg.ArchiveSessionID+".source.gz"), []byte("broken"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, _, e = s.ForCollectorPass().ReadAdmissionStage(reg.ArchiveSessionID, digest); !errors.Is(e, ErrAdmissionStageRecovery) {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(s.Home(), admissionStageDir, reg.ArchiveSessionID+".source.gz")); e != nil {
		t.Fatal("corruption quarantined evidence", e)
	}
}

func TestAdmissionStageQuotaIncludesPendingAndLeavesReservationUnadmitted(t *testing.T) {
	s, reg, b := stageFixture(t)
	path := filepath.Join(s.Home(), "pending", "synthetic-large.json")
	f, e := os.Create(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Truncate(AdmissionStageQuota / 2); e != nil {
		t.Fatal(e)
	}
	if e = f.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e = s.PrepareAdmissionStage(reg, b, "none", reg.AdmittedAt); !errors.Is(e, ErrAdmissionStageCapacity) {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(s.Home(), admissionStageDir)); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("quota allocated stage", e)
	}
}

func TestAdmissionStageCleanupFailureIsPendingAndResumesAfterRestart(t *testing.T) {
	s, reg, b := stageFixture(t)
	digest, err := s.PrepareAdmissionStage(reg, b, "none", reg.AdmittedAt)
	if err != nil {
		t.Fatal(err)
	}
	reg.AdmissionStage = digest
	if err = s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveAdmissionRequest(reg); err != nil {
		t.Fatal(err)
	}
	req, _, err := s.LoadRequest(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := s.ReadAdmissionStage(reg.ArchiveSessionID, digest)
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := archive.BuildCompressedSource(b)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.SourceObjectKey(b, compressed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	ref := archive.SourceReference{Key: key, SHA256: compressed.SHA256, CompressedBytes: len(compressed.Bytes)}
	metadata, err := archive.BuildMetadataWithAnalysis(b, archive.Analysis{}, nil, "synthetic", reg.SessionStartedAt, reg.AdmittedAt, ref, archive.ParserInfo{Name: "claude-code", Version: "synthetic"})
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
	pending, err := PreparePublication(PendingPublication{AdmissionStage: digest, Bundle: b, SourceKey: key, SourceSHA256: compressed.SHA256, SourceBytes: compressed.Bytes, MetadataKey: mk, MetadataBytes: raw}, PublicationPredecessor{State: PredecessorAbsent}, reg.DestinationID, AdmissionStageContext(reg), "synthetic-policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.SaveCommittedPublication(pending, reg.AdmittedAt); err != nil {
		t.Fatal(err)
	}
	s.onStageCleanup = func() error { return errors.New("injected cleanup failure") }
	if err = s.ReleaseAdmissionStage(reg, m, p, req.StageToken); err == nil {
		t.Fatal("cleanup failure hidden")
	}
	released, err := s.AdmissionStageReleased(reg)
	if err != nil || released {
		t.Fatal(released, err)
	}
	owed, err := s.Outstanding(reg, false)
	if err != nil || !owed.Stage || !owed.DefersExpiry() {
		t.Fatal(owed, err)
	}
	restarted := OpenReadOnly(s.Home())
	published, err := restarted.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := restarted.ResumeAdmissionStageRelease(reg, published)
	if err != nil || !resumed {
		t.Fatal(resumed, err)
	}
	released, err = restarted.AdmissionStageReleased(reg)
	if err != nil || !released {
		t.Fatal(released, err)
	}
	if _, err = os.Stat(filepath.Join(s.Home(), admissionStageDir, reg.ArchiveSessionID+".source.gz")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("released object remains", err)
	}
}

func TestAdmissionStageReconcileMissingRequestBindsExactCoveredToken(t *testing.T) {
	s, reg, b := stageFixture(t)
	digest, err := s.PrepareAdmissionStage(reg, b, "none", reg.AdmittedAt)
	if err != nil {
		t.Fatal(err)
	}
	reg.AdmissionStage = digest
	if err = s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	m, _, err := s.ReadAdmissionStage(reg.ArchiveSessionID, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcileAdmissionStage(reg, m, nil); err != nil {
		t.Fatal(err)
	}
	req, found, err := s.LoadRequest(reg.ArchiveSessionID)
	if err != nil || !found || req.StageDigest != digest || req.StageToken == "" || req.StageToken != req.Token {
		t.Fatal(req, found, err)
	}
	covered := req.StageToken
	if err = s.SaveRequest(reg.ArchiveSessionID, "sessionend", reg.AdmittedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcileAdmissionStage(reg, m, nil); err != nil {
		t.Fatal(err)
	}
	req, _, err = s.LoadRequest(reg.ArchiveSessionID)
	if err != nil || req.StageToken != covered || req.Token == covered || req.StageDigest != digest {
		t.Fatal(req, err)
	}
}

func TestAdmissionStageDirectoryCorruptionDoesNotQuarantineRegistration(t *testing.T) {
	s, reg, bundle := stageFixture(t)
	digest, err := s.PrepareAdmissionStage(reg, bundle, "none", reg.AdmittedAt)
	if err != nil {
		t.Fatal(err)
	}
	reg.AdmissionStage = digest
	if err = s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.Home(), admissionStageDir)
	moved := filepath.Join(t.TempDir(), "retained")
	if err = os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(moved, dir); err != nil {
		t.Fatal(err)
	}
	path := s.registrationPath(reg.ArchiveSessionID)
	if err = os.WriteFile(path, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	_, issues, err := s.ForCollectorPass().ScanRegistrations()
	if err != nil || !errors.Is(issues[reg.ArchiveSessionID], ErrAdmissionStageRecovery) {
		t.Fatal(issues, err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "{broken" {
		t.Fatal("registration quarantined", string(raw), err)
	}
	if _, _, err := s.ReadAdmissionStage(reg.ArchiveSessionID, digest); !errors.Is(err, ErrAdmissionStageRecovery) {
		t.Fatal(err)
	}
}

func TestAdmissionStageReleaseJournalDetectsValidJSONCorruption(t *testing.T) {
	s, reg, _ := stageFixture(t)
	reg.AdmissionStage = strings.Repeat("a", 64)
	if err := os.MkdirAll(filepath.Join(s.Home(), admissionStageDir), 0700); err != nil {
		t.Fatal(err)
	}
	path, err := s.stagePath(reg.ArchiveSessionID, ".released")
	if err != nil {
		t.Fatal(err)
	}
	r := stageRelease{Digest: reg.AdmissionStage, SourceSHA256: strings.Repeat("b", 64), CoveredToken: "synthetic-token", Complete: true}
	if err = writeStageRelease(path, r); err != nil {
		t.Fatal(err)
	}
	if complete, err := s.AdmissionStageReleased(reg); err != nil || !complete {
		t.Fatal(complete, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	r.Complete = false
	raw, err = json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdmissionStageReleased(reg); !errors.Is(err, ErrAdmissionStageRecovery) {
		t.Fatal("completion corruption accepted", err)
	}
}

func TestAdmittedStageFreezesBatchSourceAndCreationProvenance(t *testing.T) {
	for _, field := range []string{"batch", "path", "kind", "key", "provenance", "registered", "child-observation", "resolution"} {
		t.Run(field, func(t *testing.T) {
			s, reg, b := stageFixture(t)
			reg.ProjectResolution = &archive.ProjectResolution{Root: reg.ProjectRoot, Context: "reviewed"}
			digest, err := s.PrepareAdmissionStage(reg, b, "none", reg.AdmittedAt)
			if err != nil {
				t.Fatal(err)
			}
			reg.AdmissionStage = digest
			if err = s.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			_, err = s.UpdateRegistration(reg.ArchiveSessionID, func(r *archive.SessionRegistration) error {
				switch field {
				case "batch":
					r.ImportBatch = archive.NewImportBatch("different")
				case "path":
					r.TranscriptPath = "/different"
				case "kind":
					r.SourceKind = archive.SourceKindCursorSQLite
				case "key":
					r.SourceKey = "different"
				case "provenance":
					r.StartedAtSource = archive.StartedAtSourceFileCreated
				case "registered":
					r.RegisteredAt = r.RegisteredAt.Add(time.Second)
				case "resolution":
					r.ProjectResolution.Context = "changed"
				case "child-observation":
					r.SubagentObservedAt = r.AdmittedAt
				}
				return nil
			})
			if err == nil {
				t.Fatalf("mutable staged %s: %v", field, err)
			}
		})
	}
}
