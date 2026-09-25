package archive

import (
	"testing"
	"time"
)

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
