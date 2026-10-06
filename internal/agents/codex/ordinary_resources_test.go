package codex

import (
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestOrdinaryOwnTaskScannerRequiresAndReleasesSharedScratch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ref, _ := historyFile(t, dir, threadA, threadA, 0, nil, strings.Repeat("x", 1<<20))
	ledger := agentapi.NewNativeReadBudget(relatedRawBudget)
	pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{ReadBudget: ledger})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pass.Close() }()
	snap, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snap.Close() }()
	ordinary, ok := snap.(*ordinarySnapshot)
	if !ok {
		t.Fatalf("%T", snap)
	}
	usedBefore, _ := ledger.Charged()
	remaining := ledger.Available()
	if !ledger.Reserve(remaining) {
		t.Fatal("reserve")
	}
	_, err = ordinary.FirstOwnTask(t.Context())
	if !agentapi.HasFailure(err, agentapi.Unavailable) {
		t.Fatal("scanner allocated with full ledger", err)
	}
	ledger.Release(remaining)
	if _, err := ordinary.FirstOwnTask(t.Context()); err != nil {
		t.Fatal(err)
	}
	used, peak := ledger.Charged()
	if used != usedBefore || peak > relatedRawBudget {
		t.Fatal("scanner charge leaked", used, usedBefore, peak)
	}
}
