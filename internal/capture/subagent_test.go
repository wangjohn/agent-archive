package capture

import (
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/state"
)

// SubagentStop's agent_type is kept on the candidate when it is a short
// plain name, dropped when it is too long, holds anything else, or is not a
// string, and its absence changes nothing else.
func TestSubagentStopRecordsASanitizedAgentType(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		agentType any
		want      string
	}{
		{"builtin", "Explore", "Explore"},
		{"plugin", "my-plugin:reviewer", "my-plugin:reviewer"},
		{"absent", nil, ""},
		{"oversized", strings.Repeat("x", 65), ""},
		{"odd characters", "Explore; rm -rf /", ""},
		{"not a string", 42.0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home, project := t.TempDir(), t.TempDir()
			at := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
			setUpTestConfig(t, home, project, at.Add(-time.Hour))
			parentPath := writeTestTranscript(t, "parent.jsonl", "")
			if err := HandleEvent(home, "claude", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-parent", "cwd": project, "transcript_path": parentPath}, at); err != nil {
				t.Fatal(err)
			}
			stop := map[string]any{"hook_event_name": "SubagentStop", "session_id": "native-parent", "agent_id": "agent-1", "agent_transcript_path": "/never/written.jsonl"}
			if tc.agentType != nil {
				stop["agent_type"] = tc.agentType
			}
			if err := HandleEvent(home, "claude", stop, at.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			store, err := state.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			candidates, err := store.LoadSubagentCandidates()
			if err != nil || len(candidates) != 1 {
				t.Fatalf("candidates=%+v err=%v", candidates, err)
			}
			if got := candidates[0]; got.AgentType != tc.want || got.AgentID != "agent-1" || got.TranscriptPath != "/never/written.jsonl" {
				t.Fatalf("candidate=%+v, want type %q", got, tc.want)
			}
		})
	}
}
