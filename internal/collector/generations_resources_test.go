package collector

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"testing"
)

type recoveryBudgetLookup struct {
	agentapi.CodexRolloutLookup
	budget *agentapi.NativeReadBudget
}

func (l recoveryBudgetLookup) NativeReadBudget() *agentapi.NativeReadBudget { return l.budget }

func TestGenerationPreviewHoldsSharedOwnerUntilConsumerClose(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	budget := agentapi.NewNativeReadBudget(128 << 20)
	opts := scan.opts
	opts.CodexRollouts = recoveryBudgetLookup{CodexRolloutLookup: opts.CodexRollouts, budget: budget}
	build, closePreview, err := PrepareGenerationRecovery(t.Context(), scan.reg, scan.now, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer closePreview()
	owned, _ := budget.Charged()
	if owned == 0 {
		t.Fatal("preview output has no owned charge")
	}
	pressure := budget.Available()
	if !budget.Reserve(pressure) {
		t.Fatal("pressure")
	}
	if _, _, err := build(scan.reg, "next"); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("builder bypassed shared pressure", err)
	}
	used, _ := budget.Charged()
	if used != owned+pressure {
		t.Fatal("failed builder retained temporary work", used, owned+pressure)
	}
	budget.Release(pressure)
	_, pending, err := build(scan.reg, "next")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Bundle.History == nil || pending.Bundle.ArchiveSessionID != "next" {
		t.Fatal("fixed source lost")
	}
	used, _ = budget.Charged()
	if used <= owned {
		t.Fatal("returned publication has no owned charge")
	}
	closePreview()
	closePreview()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("preview close leaked", used)
	}
	if _, _, err := build(scan.reg, "next"); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatal("closed preview used released values", err)
	}
}

func TestGenerationPreviewCancellationAndInitialPressureRelease(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	budget := agentapi.NewNativeReadBudget(128 << 20)
	opts := scan.opts
	opts.CodexRollouts = recoveryBudgetLookup{CodexRolloutLookup: opts.CodexRollouts, budget: budget}
	pressure := budget.Available()
	if !budget.Reserve(pressure) {
		t.Fatal("pressure")
	}
	build, closePreview, err := PrepareGenerationRecovery(t.Context(), scan.reg, scan.now, opts)
	closePreview()
	if build != nil || !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("native preview bypassed pressure", err)
	}
	budget.Release(pressure)
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("initial refusal leaked", used)
	}
	ctx, cancel := context.WithCancel(t.Context())
	build, closePreview, err = PrepareGenerationRecovery(ctx, scan.reg, scan.now, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer closePreview()
	cancel()
	if _, _, err := build(scan.reg, "next"); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled preview built publication", err)
	}
	closePreview()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("canceled preview leaked", used)
	}
	// A changed registration also refuses before accumulating successor output.
	build, closePreview, err = PrepareGenerationRecovery(t.Context(), scan.reg, scan.now, opts)
	if err != nil {
		t.Fatal(err)
	}
	changed := scan.reg
	changed.NativeSessionID = "different"
	if _, _, err := build(changed, "next"); err == nil {
		t.Fatal("changed registration accepted")
	}
	closePreview()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("error cleanup leaked", used)
	}
}
