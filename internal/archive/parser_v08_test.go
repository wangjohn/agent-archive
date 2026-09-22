package archive

import (
	"bytes"
	"strings"
	"testing"
)

// Claude Code writes a background task's completion as a user record whose
// origin.kind is "task-notification". It is not a prompt; the person's own
// prompt carries origin.kind "human" and still is one.
func TestParserV08TaskNotificationIsNotAPrompt(t *testing.T) {
	view, metadata := parsedFixture(t, "claude", "claude-task-notification.jsonl")
	countIs(t, "turns", metadata.Counts.Turns, 1)
	kinds := turnKinds(view)
	if kinds[0] != TurnKindHumanPrompt || kinds[2] != TurnKindHarnessNotification {
		t.Fatalf("turn kinds = %#v", kinds)
	}
}

// Filter 5 keeps origin only as its kind string and promptSource only as a
// string; every other origin member is omitted and reported by name.
func TestFilterV5RetainsOnlyOriginKindAndPromptSource(t *testing.T) {
	filtered, err := ClaudeAdapter{}.FilterJSONL(bytes.NewReader(fixture(t, "claude-task-notification.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	notification := string(filtered.Records[2])
	if !strings.Contains(notification, `"origin":{"kind":"task-notification"}`) || !strings.Contains(notification, `"promptSource":"system"`) {
		t.Fatalf("notification record = %s", notification)
	}
	if strings.Contains(notification, "a87a9b2a83c46c982\"}") || strings.Contains(notification, "taskId") {
		t.Fatalf("origin member other than kind retained: %s", notification)
	}
	omitted := false
	for _, gap := range filtered.Gaps {
		if gap.Code == "unknown_field_omitted" && strings.Contains(gap.Detail, "taskId") {
			omitted = true
		}
	}
	if !omitted {
		t.Fatalf("taskId not reported as omitted: %#v", filtered.Gaps)
	}
}

// A non-object origin, or one without a string kind, is omitted whole.
func TestFilterV5DropsMalformedOrigin(t *testing.T) {
	input := `{"type":"user","uuid":"u1","origin":"human","message":{"role":"user","content":"hi"}}
{"type":"user","uuid":"u2","origin":{"kind":7},"promptSource":{"x":1},"message":{"role":"user","content":"hi"}}
`
	filtered, err := ClaudeAdapter{}.FilterJSONL(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range filtered.Records {
		if strings.Contains(string(record), "origin") || strings.Contains(string(record), "promptSource") {
			t.Fatalf("malformed origin or promptSource retained: %s", record)
		}
	}
}

// Current Codex writes function and custom tool output as a list of
// input_text blocks. Its size must be measured, not reported as zero, and
// the injected <recommended_plugins> catalog must not become a prompt.
func TestParserV08CodexListOutputAndInjectedPlugins(t *testing.T) {
	view, metadata := parsedFixture(t, "codex", "codex-list-output.jsonl")
	countIs(t, "turns", metadata.Counts.Turns, 1)
	countIs(t, "tool calls", metadata.Counts.ToolCalls, 2)
	countIs(t, "tool results", metadata.Counts.ToolResults, 2)
	for _, call := range view.ToolCalls {
		if call.OutputBytes == nil || *call.OutputBytes == 0 {
			t.Fatalf("call %s output bytes = %v", call.CallID, call.OutputBytes)
		}
	}
	for _, turn := range view.Turns {
		if strings.Contains(turn.Text, "Airtable") || strings.Contains(turn.Text, "recommended_plugins") {
			t.Fatalf("plugin catalog retained as %s: %q", turn.Kind, turn.Text)
		}
	}
}
