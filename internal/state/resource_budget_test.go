package state

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestLocalBudgetRefusalPreservesPendingAndIndependentProgress(t *testing.T) {
	s := newTestStore(t).ForCollectorPass()
	raw := []byte(`{"request_token":"newer-request","source_bytes":"cHJlc2VydmVk"}`)
	path := s.pendingPath("owed")
	if err := local.WriteBytes(path, raw); err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(2 * int64(len(raw)))
	pressure := int64(1)
	if !budget.Reserve(pressure) {
		t.Fatal("pressure reservation")
	}
	scoped, closeScope := s.WithReadBudget(t.Context(), budget)
	if _, _, err := scoped.LoadPending("owed"); !agentapi.HasFailure(err, agentapi.Limit) || errors.Is(err, ErrQuarantined) {
		t.Fatalf("refusal %v", err)
	}
	closeScope()
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(raw) {
		t.Fatalf("journal changed: %s %v", after, err)
	}
	if len(s.QuarantinedFiles()) != 0 {
		t.Fatal("valid work quarantined")
	}
	budget.Release(pressure)
	scoped, closeScope = s.WithReadBudget(t.Context(), budget)
	p, found, err := scoped.LoadPending("owed")
	if err != nil || !found || p.RequestToken != "newer-request" {
		t.Fatalf("retry %#v %t %v", p, found, err)
	}
	used, _ := budget.Charged()
	if used != int64(len(raw)) {
		t.Fatalf("decoded lifetime charged %d", used)
	}
	closeScope()
	closeScope()
	used, _ = budget.Charged()
	if used != 0 {
		t.Fatalf("close leaked %d", used)
	}
}
func TestLocalBudgetCancelAndEncodingRefusalRelease(t *testing.T) {
	s := newTestStore(t).ForCollectorPass()
	path := s.pendingPath("cancel")
	if err := local.WriteBytes(path, []byte(`{"request_token":"owed"}`)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	budget := agentapi.NewNativeReadBudget(1 << 20)
	scoped, closeScope := s.WithReadBudget(ctx, budget)
	if _, _, err := scoped.LoadPending("cancel"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	closeScope()
	scoped, closeScope = s.WithReadBudget(t.Context(), budget)
	if err := scoped.writeCompact(s.pendingPath("unsupported"), make(chan int)); !agentapi.HasFailure(err, agentapi.Limit) {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.pendingPath("unsupported")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	closeScope()
	used, _ := budget.Charged()
	if used != 0 {
		t.Fatalf("refusal leaked %d", used)
	}
}
