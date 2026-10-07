package state

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestPendingQuotaReceiptCrashReconcilesOnlyTarget(t *testing.T) {
	t.Parallel()
	s, reg, p := saturatedStagePending(t)
	s.onQuotaReceipt = func() error { return errors.New("injected after pending before receipt") }
	if err := s.SavePending(reg.ArchiveSessionID, p); err == nil {
		t.Fatal("interruption hidden")
	}
	if _, err := s.admissionStageUsage(); !errors.Is(err, ErrAdmissionStageCapacity) {
		t.Fatal("unreceipted copies were credited", err)
	}
	restarted := OpenReadOnly(s.Home())
	reads := map[string]int{}
	restarted.onQuotaBodyRead = func(kind string) { reads[kind]++ }
	if err := restarted.SavePending(reg.ArchiveSessionID, p); err != nil {
		t.Fatal("target reconciliation made no progress", err)
	}
	if reads["pending"] != 1 || reads["stage"] != 2 {
		t.Fatal("unexpected target work", reads)
	}
	reads = map[string]int{}
	if _, err := restarted.admissionStageUsage(); err != nil || len(reads) != 0 {
		t.Fatal("aggregate opened bodies", reads, err)
	}
	info, err := os.Stat(restarted.pendingPath(reg.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(restarted.pendingPath(reg.ArchiveSessionID), info.ModTime().Add(1), info.ModTime().Add(1)); err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.admissionStageUsage(); !errors.Is(err, ErrAdmissionStageCapacity) {
		t.Fatal("stale stat epoch credited", err)
	}
	if err = restarted.SavePending(reg.ArchiveSessionID, p); err != nil {
		t.Fatal("stat receipt could not reconcile", err)
	}
	if err = restarted.RemovePending(reg.ArchiveSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(restarted.quotaReceiptPath(reg.ArchiveSessionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("receipt cleanup retained credit", err)
	}
}

func TestPendingQuotaUnrelatedWritesNeverReadRetainedBodies(t *testing.T) {
	t.Parallel()
	s, reg, bundle := stageFixture(t)
	for i := 0; i < 40; i++ {
		r := reg
		r.ArchiveSessionID = fmt.Sprintf("synthetic-%d", i)
		r.NativeSessionID = fmt.Sprintf("native-%d", i)
		b := bundle
		b.ArchiveSessionID = r.ArchiveSessionID
		b.NativeSessionID = r.NativeSessionID
		digest, err := s.PrepareAdmissionStage(r, b, "none", r.AdmittedAt)
		if err != nil {
			t.Fatal(err)
		}
		r.AdmissionStage = digest
		if err = s.SaveRegistration(r); err != nil {
			t.Fatal(err)
		}
	}
	reads := 0
	s.onQuotaBodyRead = func(string) { reads++ }
	workspace, err := NewTemporaryReservation(s, PublicationPrivacy, "unrelated")
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	for range 4 {
		if err = workspace.Reserve(1024); err != nil {
			t.Fatal(err)
		}
	}
	p := PendingPublication{Bundle: archive.SourceBundle{}, SourceKey: "source", MetadataKey: "metadata", SourceSHA256: "synthetic", SourceBytes: []byte("synthetic"), MetadataBytes: []byte("synthetic")}
	for range 4 {
		if err = s.SavePending("unrelated", p); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 0 {
		t.Fatal("unrelated operation decoded history", reads)
	}
	t.Logf("40 retained stages, 4 scratch reserves, 4 unrelated atomic pending replacements: source/pending body reads=%d", reads)
}
