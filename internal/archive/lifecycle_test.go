package archive

import (
	"maps"
	"strings"
	"testing"
	"time"
)

func TestLifecycleDerivationIsOrderedConservativeAndDeterministic(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	event := func(at time.Time, name string, fields map[string]any) SupplementalEvidence {
		payload := map[string]any{"event_name": name}
		maps.Copy(payload, fields)
		return SupplementalEvidence{Kind: EvidenceKindLifecycleHook, ObservedAt: at, Provenance: "hook:fixture:" + strings.ToLower(name), Payload: payload}
	}
	// Deliberately append out of order and repeat an exact event. The delayed
	// event must not overwrite the later state, and retries must be inert.
	evidence := []SupplementalEvidence{
		event(now.Add(2*time.Minute), "Stop", nil),
		event(now, "SessionStart", nil),
		event(now.Add(time.Minute), "Stop", nil),
		event(now.Add(2*time.Minute), "Stop", nil),
		event(now.Add(3*time.Minute), "UserPromptSubmit", nil),
	}
	state, outcome := deriveLifecycle(evidence)
	if state != MetadataStateActive || outcome != TurnOutcomeUnknown {
		t.Fatalf("active transition = %q/%q", state, outcome)
	}
	state, outcome = deriveLifecycle(append(evidence, event(now.Add(4*time.Minute), "Stop", nil)))
	if state != MetadataStateIdle || outcome != TurnOutcomeUnknown {
		t.Fatalf("generic stop must be idle with unknown outcome, got %q/%q", state, outcome)
	}
	state, outcome = deriveLifecycle(append(evidence, event(now.Add(4*time.Minute), "Stop", map[string]any{"status": "completed"})))
	if state != MetadataStateIdle || outcome != TurnOutcomeUnknown {
		t.Fatalf("generic stop status must not manufacture completion, got %q/%q", state, outcome)
	}
	cursorStop := event(now.Add(4*time.Minute), "Stop", map[string]any{"status": "completed"})
	cursorStop.Provenance = "hook:cursor:stop"
	state, outcome = deriveLifecycle(append(evidence, cursorStop))
	if state != MetadataStateIdle || outcome != TurnOutcomeCompleted {
		t.Fatalf("documented Cursor stop completion = %q/%q", state, outcome)
	}
	// A subagent finishing does not close, idle, or complete the parent
	// session, whatever status it reports and from whichever provenance.
	for _, status := range []string{"incomplete", "completed"} {
		state, outcome = deriveLifecycle(append(evidence, event(now.Add(4*time.Minute), "SubagentStop", map[string]any{"status": status})))
		if state != MetadataStateActive || outcome != TurnOutcomeUnknown {
			t.Fatalf("subagent stop with status %q changed the parent session to %q/%q", status, state, outcome)
		}
	}
	cursorSubagent := event(now.Add(4*time.Minute), "SubagentStop", map[string]any{"status": "completed"})
	cursorSubagent.Provenance = "hook:cursor:subagentstop"
	state, outcome = deriveLifecycle(append(evidence, cursorStop, cursorSubagent))
	if state != MetadataStateIdle || outcome != TurnOutcomeCompleted {
		t.Fatalf("subagent stop after a documented stop must leave it as observed, got %q/%q", state, outcome)
	}
	for name, want := range map[string]TurnOutcome{"Interrupt": TurnOutcomeInterrupted, "StopFailure": TurnOutcomeError} {
		state, outcome = deriveLifecycle(append(evidence, event(now.Add(5*time.Minute), name, nil)))
		if state != MetadataStateIdle || outcome != want {
			t.Fatalf("%s = %q/%q", name, state, outcome)
		}
	}
	state, outcome = deriveLifecycle(append(evidence, event(now.Add(6*time.Minute), "SessionEnd", nil)))
	if state != MetadataStateClosed || outcome != TurnOutcomeUnknown {
		t.Fatalf("closure = %q/%q", state, outcome)
	}
}
