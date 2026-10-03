package nativecodec

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// transcriptBundle filters a Claude fixture from testdata into a bundle,
// the path a capture takes.
func transcriptBundle(t *testing.T, name string) SourceBundle {
	t.Helper()
	filtered, err := (ClaudeAdapter{}).FilterJSONL(bytes.NewReader(fixture(t, name)))
	if err != nil {
		t.Fatal(err)
	}
	reg := registration()
	reg.Harness = Harness{Name: "claude"}
	bundle, err := NewSourceBundle(reg, ClaudeAdapter{}, filtered, time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

// transcriptOutline is one line per exchange and step, for comparing.
func transcriptOutline(t *testing.T, bundle SourceBundle) string {
	t.Helper()
	transcript, err := BuildTranscript(bundle, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, exchange := range transcript.Exchanges {
		fmt.Fprintf(&b, "%s %q\n", exchange.Kind, firstLine(exchange.Text, 40))
		for _, step := range exchange.Steps {
			text := step.Text
			if step.Tool != nil {
				text = step.Tool.Name + " " + step.Tool.Summary
			}
			fmt.Fprintf(&b, "  %s %q", step.Kind, firstLine(text, 40))
			if step.Output != "" {
				fmt.Fprintf(&b, " -> %q", step.Output)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// A transcript shows what the person saw: shell commands and their output,
// local commands and theirs, compaction summaries, and app notices, each
// where it happened. A notice starts its own exchange, so the reply to it is
// not credited to the prompt before.
func TestTranscriptShowsWhatThePersonSaw(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"claude-shell-command.jsonl": `leading ""
  shell "git status" -> "On branch main\nnothing to commit, working tree clean"
prompt "Commit nothing, just confirm the branch."
  text "You are on main with a clean tree."
`,
		"claude-local-command.jsonl": `leading ""
  command "/model" -> "Set model to claude-opus-5"
prompt "Summarize the widget package."
  text "It parses widget files."
`,
		"claude-task-notification.jsonl": `prompt "Review the collector package in the back…"
  text "Started a background reviewer."
notification "Background task completed"
  text "The reviewer finished; two findings."
`,
	} {
		if got := transcriptOutline(t, transcriptBundle(t, name)); got != want {
			t.Errorf("%s:\n%s\nwant:\n%s", name, got, want)
		}
	}
	got := transcriptOutline(t, transcriptBundle(t, "claude-compaction.jsonl"))
	if !strings.Contains(got, `command "/compact" -> "Compacted. ctrl+o to see full summary"`) || strings.Count(got, "  summary ") != 2 {
		t.Errorf("compaction:\n%s", got)
	}
}

// Only a harness that never records tool results (Cursor) says so; a Claude
// or Codex session whose calls have no results yet does not.
func TestTranscriptToolResultsUnavailableOnlyForCursor(t *testing.T) {
	t.Parallel()
	call := map[string]any{"type": "assistant", "uuid": "a1", "timestamp": "2026-09-22T10:00:01Z", "message": map[string]any{"id": "m1", "role": "assistant", "content": []any{
		map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "ls"}},
	}}}
	for _, harness := range []string{"claude", "codex"} {
		bundle := testBundle(harness, call)
		if harness == "codex" {
			bundle = testBundle(harness, map[string]any{"type": "response_item", "timestamp": "2026-09-22T10:00:01Z", "payload": map[string]any{"type": "function_call", "name": "shell", "call_id": "c1", "arguments": `{"command":["ls"]}`}})
		}
		transcript, err := BuildTranscript(bundle, HandoffOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(transcript.Exchanges) != 1 || transcript.Exchanges[0].Steps[0].Tool == nil || transcript.ToolResultsUnavailable {
			t.Fatalf("%s transcript = %#v", harness, transcript)
		}
	}
	cursor := handoffBundle(t, "cursor")
	h, err := BuildHandoff(cursor, nil, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := BuildTranscript(cursor, HandoffOptions{})
	if err != nil || transcript.ToolResultsUnavailable != h.ToolResultsUnavailable {
		t.Fatalf("cursor: %v %v", transcript.ToolResultsUnavailable, err)
	}
}

// The handoff fixtures' prompts come through as handoff arranges them.
func TestTranscriptKeepsHandoffPrompts(t *testing.T) {
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
		var prompts, want []string
		for _, e := range transcript.Exchanges {
			if e.Kind == TranscriptExchangePrompt {
				prompts = append(prompts, e.Text)
			}
		}
		for _, e := range h.Exchanges {
			if e.Prompt != "" {
				want = append(want, e.Prompt)
			}
		}
		if !reflect.DeepEqual(prompts, want) {
			t.Fatalf("%s: prompts %q, want %q", harness, prompts, want)
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
	if exchange.Text != "line one]52;c;evil\nline two" || strings.ContainsAny(exchange.Steps[0].Text, "\u202e") {
		t.Fatalf("not display text: %q %q", exchange.Text, exchange.Steps[0].Text)
	}
}
