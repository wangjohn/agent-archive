package archive

import (
	"reflect"
	"testing"
	"time"
)

func TestSummarizeTurnsDeduplicatesStreamedAssistantAndSortsModels(t *testing.T) {
	t.Parallel()
	turns := []NormalizedTurn{
		{Kind: TurnKindHumanPrompt, Role: "user", Model: "z", Text: "hello"},
		{Kind: TurnKindAssistant, Role: "assistant", MessageID: "response", Model: "b"},
		{Kind: TurnKindAssistant, Role: "assistant", MessageID: "response", Model: "b"},
		{Kind: TurnKindShellCommand, Role: "user", Model: "ignored"},
		{Kind: TurnKindToolResult, Role: "user", Model: "ignored"},
		{Kind: TurnKindAssistant, Role: "assistant", ResponseModel: "a"},
	}
	prompts, messages, shellCommands, models := summarizeTurns(turns)
	if prompts != 1 || messages != 3 || shellCommands != 1 {
		t.Fatalf("counts = %d/%d/%d", prompts, messages, shellCommands)
	}
	if len(models) != 3 || models[0].Attributes["gen_ai.request.model"] != "b" ||
		models[1].Attributes["gen_ai.request.model"] != "z" ||
		models[2].Attributes["gen_ai.response.model"] != "a" {
		t.Fatalf("model order or attribution changed: %#v", models)
	}
	for _, model := range models {
		if model.TurnCount == nil || *model.TurnCount != 1 {
			t.Fatalf("unexpected model turn count: %#v", model)
		}
	}
	if !reflect.DeepEqual(turns[1], NormalizedTurn{Kind: TurnKindAssistant, Role: "assistant", MessageID: "response", Model: "b"}) {
		t.Fatal("summary mutated a normalized turn")
	}
}

func TestStructuredCountsKeepsMissingTokensUnknownAndPrefersCompactionBoundaries(t *testing.T) {
	t.Parallel()
	view := NormalizedView{CompactBoundaries: 2, CompactSummaries: 3}
	counts := analyzedCounts(Analysis{View: view, Observability: Observability{StructuredCounts: Availability{State: AvailabilityAvailable}}}, 1, 2, 3)
	if counts.InputTokens != nil || counts.OutputTokens != nil || counts.Compactions != nil {
		t.Fatalf("unexpected unobserved counts: %#v", counts)
	}
	// A Claude bundle from a version that observes compactions must count
	// boundaries rather than counting both the boundary and summary records.
	analysis := Analysis{View: view, Observability: Observability{StructuredCounts: Availability{State: AvailabilityAvailable}, Compactions: Availability{State: AvailabilityAvailable}}}
	counts = analyzedCounts(analysis, 1, 2, 3)
	if counts.Compactions == nil || *counts.Compactions != 2 {
		t.Fatalf("compactions = %v", counts.Compactions)
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestSessionClosurePreservesLastObservedTurnOutcome(t *testing.T) {
	t.Parallel()
	at := time.Now().UTC()
	for _, tc := range []struct {
		event      string
		provenance string
		//lint:ignore LV1001 raw hook payload text under test, not a closed set
		status string
		want   TurnOutcome
	}{
		{"StopFailure", "hook:claude:stopfailure", "", TurnOutcomeError},
		{"Interrupt", "hook:codex:interrupt", "", TurnOutcomeInterrupted},
		{"stop", "hook:cursor:stop", "completed", TurnOutcomeCompleted},
	} {
		events := []SupplementalEvidence{
			{Kind: EvidenceKindLifecycleHook, ObservedAt: at, Provenance: tc.provenance, Payload: map[string]any{"event_name": tc.event, "status": tc.status}},
			{Kind: EvidenceKindLifecycleHook, ObservedAt: at.Add(time.Second), Provenance: "hook:claude:sessionend", Payload: map[string]any{"event_name": "SessionEnd"}},
		}
		state, outcome := deriveLifecycle(events)
		if state != MetadataStateClosed || outcome != tc.want {
			t.Fatalf("%s then close = %s/%s", tc.event, state, outcome)
		}
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestSkillUseRetainsMultipleHashesForSameName(t *testing.T) {
	t.Parallel()
	var metadata Metadata
	deriveSkills(SourceBundle{}, []SkillUse{
		{Name: "review", SHA256: "aaa", Evidence: SkillUseEvidenceNativeInvocation},
		{Name: "review", SHA256: "bbb", Evidence: SkillUseEvidenceNativeInvocation},
		{Name: "review", Evidence: SkillUseEvidenceReadInference},
	}, &metadata)
	if len(metadata.SkillsUsed) != 2 || metadata.SkillsUsed[0].SHA256 != "aaa" || metadata.SkillsUsed[1].SHA256 != "bbb" {
		t.Fatalf("lost skill version: %#v", metadata.SkillsUsed)
	}
}
