package retention

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type orphanStageAdapter struct{}

func (orphanStageAdapter) Name() string { return "claude-code" }

func (orphanStageAdapter) Version() string { return "synthetic" }

func TestSweepReportsSourceOnlyAdmissionStageAfterRestart(t *testing.T) {
	local := newTestStore(t)
	reg := registration("source.only", "")
	reg.Harness = archive.Harness{Name: "claude-code"}
	reg.Origin = archive.SessionOriginImport
	reg.ImportBatch = archive.NewImportBatch("synthetic")
	reg.AdmittedAt = reg.RegisteredAt
	b, err := archive.NewSourceBundle(reg, orphanStageAdapter{}, archive.FilteredTranscript{Format: "jsonl", Records: [][]byte{[]byte(`{"type":"user","message":{"role":"user","content":"synthetic"}}`)}}, reg.Admitted(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = local.PrepareAdmissionStage(reg, b, "none", reg.Admitted()); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(local.Home(), "admission-stages", reg.ArchiveSessionID+".json")); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(local.Home(), "admission-stages", reg.ArchiveSessionID+".source.gz")
	before, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := state.Open(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	owed, err := restarted.HasDurableSessionEvidence(reg.ArchiveSessionID)
	if err != nil || !owed {
		t.Fatal(owed, err)
	}
	result := sweep(t, restarted, storagetest.NewMemoryStore(), reg.Admitted().Add(365*24*time.Hour), Options{})
	if !errors.Is(result.Errors[reg.ArchiveSessionID], state.ErrAdmissionStageRecovery) {
		t.Fatalf("Sweep omitted source-only durable recovery: %#v", result)
	}
	for range 2 {
		after, err := os.ReadFile(sourcePath)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("sweep changed retained stage", err)
		}
		regs, err := restarted.LoadRegistrations()
		if err != nil || len(regs) != 0 {
			t.Fatal("sweep invented registration", regs, err)
		}
		if result.DeletedSnapshots != 0 || len(result.DeletedSessions) != 0 || len(result.PrunedSessions) != 0 {
			t.Fatalf("recovery deleted data: %#v", result)
		}
		if _, err := os.Stat(filepath.Join(local.Home(), "session-deletions", reg.ArchiveSessionID+".json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("sweep invented deletion authority", err)
		}
		result = sweep(t, restarted, storagetest.NewMemoryStore(), reg.Admitted().Add(366*24*time.Hour), Options{})
		if !errors.Is(result.Errors[reg.ArchiveSessionID], state.ErrAdmissionStageRecovery) {
			t.Fatalf("repeated sweep lost obligation: %#v", result)
		}
	}
}
