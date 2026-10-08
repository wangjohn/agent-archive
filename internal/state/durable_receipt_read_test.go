package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestCompletedGenerationReceiptReadEligibilityAndCost(t *testing.T) {
	s := newTestStore(t)
	if err := config.WithDurableStorage(s.home, func(config.DurableStorageGuard) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.home, generationRecoveryDir), 0700); err != nil {
		t.Fatal(err)
	}
	path := s.generationRecoveryPath("previous")
	complete := `{"version":1,"key":{"Agent":"codex","NativeID":"native"},"previous":"previous","next":"next","complete":true,"registration":null,"pending":null,"request":null}`
	if err := os.WriteFile(path, []byte(complete), 0600); err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(1 << 20)
	scoped, closeScope := s.WithReadBudget(t.Context(), budget)
	defer closeScope()
	before := budget.Available()
	for range 20 {
		if err := scoped.CheckDurableSessionRead("previous"); err != nil {
			t.Fatal(err)
		}
		if err := scoped.ResumeGenerationRecoveries(t.Context()); err != nil {
			t.Fatal(err)
		}
		if budget.Available() != before {
			t.Fatal("receipt guard retained scratch")
		}
	}
	if s.durableInspection.configLoads != 1 || s.durableInspection.scans != 1 {
		t.Fatalf("repeated global/config decode: %+v", s.durableInspection)
	}
	for _, suffix := range []string{`,"future":null}`, `,"request":{}}`} {
		raw := `{"version":1,"key":{"Agent":"codex","NativeID":"native"},"previous":"previous","next":"next","complete":true` + suffix
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if err := scoped.CheckDurableSessionRead("previous"); !errors.Is(err, ErrDurableStorageRecovery) {
			t.Fatal("unsupported receipt read", err)
		}
		if err := scoped.ResumeGenerationRecoveries(t.Context()); !errors.Is(err, ErrDurableStorageRecovery) {
			t.Fatal("unsupported receipt replay", err)
		}
		if actual, err := os.ReadFile(path); err != nil || string(actual) != raw || budget.Available() != before {
			t.Fatal("receipt changed/leaked", err)
		}
	}
}
