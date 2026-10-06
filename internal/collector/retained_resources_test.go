package collector

import (
	"errors"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestRetainedRefilterMakesSmallProgressUnderSharedPressure(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	read, ok, err := scan.read()
	if err != nil || !ok {
		t.Fatal(err)
	}
	bundle, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	scan.releaseRetained()
	budget := agentapi.NewNativeReadBudget(2 << 20)
	pressure := int64(1 << 20)
	if !budget.Reserve(pressure) {
		t.Fatal("pressure")
	}
	scan.retainedBudget = budget
	out, err := scan.refilterRetained(codex.Filter{}, bundle)
	if err != nil {
		t.Fatalf("small valid work starved by ceiling: %v", err)
	}
	if err := out.ValidateHistory(); err != nil {
		t.Fatal(err)
	}
	if len(out.NativeRecords) != len(bundle.NativeRecords) {
		t.Fatal("partial refilter output")
	}
	used, peak := budget.Charged()
	if used <= pressure || peak >= 2<<20 {
		t.Fatalf("owned charge %d peak%d", used, peak)
	}
	scan.releaseRetained()
	used, _ = budget.Charged()
	if used != pressure {
		t.Fatalf("release imbalance %d", used-pressure)
	}
	// Complete exhaustion refuses without retaining output, and does not prevent
	// independent small progress when the unrelated reservation is returned.
	fill := budget.Available()
	if !budget.Reserve(fill) {
		t.Fatal("fill")
	}
	_, err = scan.refilterRetained(codex.Filter{}, bundle)
	if !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatalf("refusal %v", err)
	}
	budget.Release(fill)
	budget.Release(pressure)
	if _, err := scan.refilterRetained(codex.Filter{}, bundle); err != nil {
		t.Fatal(err)
	}
	scan.releaseRetained()
	used, _ = budget.Charged()
	if used != 0 {
		t.Fatalf("independent progress leaked%d", used)
	}
}

func TestRetainedComparisonReservationReleasesOnRefusal(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	budget := agentapi.NewNativeReadBudget(32 << 10)
	scan.retainedBudget = budget
	_, err := scan.jsonEncodingsEqual(archive.SupplementalEvidence{Payload: map[string]any{"text": "synthetic"}}, nil)
	if !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal(err)
	}
	scan.releaseRetained()
	used, _ := budget.Charged()
	if used != 0 {
		t.Fatalf("comparison refusal leaked%d", used)
	}
}
