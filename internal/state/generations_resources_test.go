package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"os"
	"testing"
)

func TestGenerationValidationAndJournalPressurePreserveRetry(t *testing.T) {
	s, reg, at := generationFixture(t)
	next, pending, err := generationBuilder(at)(reg, "next-generation")
	if err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(1)
	if err := validateGenerationPublication(next, pending, budget, t.Context()); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("validation bypassed pressure", err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("validation leaked", used)
	}
	budget = agentapi.NewNativeReadBudget(4 << 20)
	if err := validateGenerationPublication(next, pending, budget, t.Context()); err != nil {
		t.Fatal(err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("validation scratch leaked", used)
	}
	path := s.generationRecoveryPath(reg.ArchiveSessionID)
	journal := generationRecovery{Version: 1, Previous: reg.ArchiveSessionID, Next: next.ArchiveSessionID, Pending: &pending, Registration: &next}
	if err := local.Write(path, journal); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	budget = agentapi.NewNativeReadBudget(2*int64(len(before)) - 1)
	scoped, closeScope := s.WithReadBudget(t.Context(), budget)
	if _, _, err := scoped.loadGenerationRecovery(reg.ArchiveSessionID); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("journal bypassed pressure", err)
	}
	closeScope()
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("pressure changed journal", err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("journal refusal leaked", used)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := validateGenerationPublication(next, pending, nil, ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("validation ignored cancellation", err)
	}
}

func TestGenerationJournalRefusesHistoryAroundOrdinaryBundle(t *testing.T) {
	_, reg, at := generationFixture(t)
	next, pending, err := generationBuilder(at)(reg, "next-generation")
	if err != nil {
		t.Fatal(err)
	}
	pending.History = &PendingHistory{Version: 1}
	var metadata archive.Metadata
	if err := json.Unmarshal(pending.MetadataBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.SchemaVersion = archive.HistoryMetadataSchemaVersion
	metadata.History = &archive.RevisionHistory{CurrentRevision: "11111111-1111-4111-8111-111111111111"}
	pending.MetadataBytes, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateGenerationPublication(next, pending, nil, t.Context()); err == nil {
		t.Fatal("ordinary bundle admitted as recovery history")
	}
}

func TestMalformedGenerationHistoryJournalRemainsUntouched(t *testing.T) {
	s, reg, at := generationFixture(t)
	interrupted := errors.New("stop after durable generation journal")
	s.onIndexStep = func(step string) error {
		if step == "generation-journal" {
			return interrupted
		}
		return nil
	}
	if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, generationBuilder(at)); !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	journal, found, err := s.loadGenerationRecovery(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(err)
	}
	journal.Pending.History = &PendingHistory{Version: 1}
	var metadata archive.Metadata
	if err := json.Unmarshal(journal.Pending.MetadataBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.SchemaVersion = archive.HistoryMetadataSchemaVersion
	metadata.History = &archive.RevisionHistory{CurrentRevision: "11111111-1111-4111-8111-111111111111"}
	journal.Pending.MetadataBytes, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	path := s.generationRecoveryPath(reg.ArchiveSessionID)
	if err := local.Write(path, journal); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s.onIndexStep = nil
	if err := s.ResumeGenerationRecoveries(t.Context()); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		t.Fatal("malformed history allowed routing mutation", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("malformed journal was changed", err)
	}
	original, found, err := s.LoadRegistration(reg.ArchiveSessionID)
	if err != nil || !found || original.CaptureFrozen {
		t.Fatal("original changed", err)
	}
	if _, found, err := s.LoadRegistration(journal.Next); err != nil || found {
		t.Fatal("malformed journal registered successor", err)
	}
}
