package archive

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// A transcript is the handoff's exchanges without a budget.
func TestTranscriptMatchesHandoffExchanges(t *testing.T) {
	t.Parallel()
	for _, harness := range []string{"claude", "codex", "cursor"} {
		bundle := handoffBundle(t, harness)
		h, err := BuildHandoff(bundle, nil, HandoffOptions{})
		if err != nil {
			t.Fatal(err)
		}
		transcript, err := BuildTranscript(bundle, HandoffOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(transcript.Exchanges, h.Exchanges) {
			t.Fatalf("%s: transcript exchanges differ from handoff's:\n%#v\n%#v", harness, transcript.Exchanges, h.Exchanges)
		}
		if transcript.ToolResultsUnavailable != h.ToolResultsUnavailable {
			t.Fatalf("%s: tool results unavailable = %v", harness, transcript.ToolResultsUnavailable)
		}
	}
}

// A hook's final response is shown only when the transcript lacks it: not a
// subagent's, not one matched by ID, and not one that repeats a reply.
func TestTranscriptHookFinals(t *testing.T) {
	t.Parallel()
	bundle := handoffBundle(t, "claude")
	final := func(payload map[string]any) SupplementalEvidence {
		return SupplementalEvidence{Kind: EvidenceKindFinalResponse, ObservedAt: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC), Provenance: "hook:claude:stop", Payload: payload}
	}
	bundle.SupplementalEvidence = []SupplementalEvidence{
		final(map[string]any{"text": "Tests pass now. Next: add a regression test for Size."}),
		final(map[string]any{"text": "Subagent done.", "agent_id": "agent-1"}),
		final(map[string]any{"text": "Matched by ID.", "message_id": "a1"}),
		final(map[string]any{"text": "Only the hook saw this \x1b[31mreply\x1b[0m."}),
	}
	transcript, err := BuildTranscript(bundle, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.HookFinals) != 1 || transcript.HookFinals[0] != "Only the hook saw this [31mreply[0m." {
		t.Fatalf("hook finals = %q", transcript.HookFinals)
	}
}

// Every transcript string is display text: escape sequences are removed,
// line breaks kept.
func TestTranscriptIsDisplayText(t *testing.T) {
	t.Parallel()
	bundle := testBundle("claude",
		map[string]any{"type": "user", "uuid": "u1", "timestamp": "2026-09-22T10:00:00Z", "message": map[string]any{"role": "user", "content": "line one\x1b]52;c;evil\x07\r\nline two"}},
		map[string]any{"type": "assistant", "uuid": "a1", "timestamp": "2026-09-22T10:00:01Z", "message": map[string]any{"id": "m1", "role": "assistant", "content": []any{map[string]any{"type": "text", "text": "reply\u202e"}}}},
	)
	transcript, err := BuildTranscript(bundle, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Exchanges) != 1 {
		t.Fatalf("exchanges = %#v", transcript.Exchanges)
	}
	exchange := transcript.Exchanges[0]
	if exchange.Prompt != "line one]52;c;evil\nline two" || strings.ContainsAny(exchange.Steps[0].Text, "\u202e") {
		t.Fatalf("not display text: %q %q", exchange.Prompt, exchange.Steps[0].Text)
	}
}
