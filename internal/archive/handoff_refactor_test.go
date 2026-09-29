package archive

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHandoffSessionSelectsModelsAndRecordedTimes(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 9, 1, 8, 0, 0, 0, time.FixedZone("west", -7*3600))
	later := started.Add(2 * time.Hour)
	view := NormalizedView{Turns: []NormalizedTurn{
		{Model: "model-a", ResponseModel: "model-b", Timestamp: later.Format(time.RFC3339Nano)},
		{Model: "model-a", Timestamp: started.Format(time.RFC3339Nano)},
	}}
	metadata := &Metadata{
		Models:     []ModelSummary{{Attributes: map[string]string{"gen_ai.response.model": "model-b", "gen_ai.request.model": "model-c"}}},
		StartedAt:  started.Add(-time.Hour),
		CapturedAt: later.Add(time.Hour),
	}
	session := handoffSession(SourceBundle{}, view, metadata, HandoffOptions{StartedAt: later.Add(2 * time.Hour)})
	if !reflect.DeepEqual(session.Models, []string{"model-a", "model-b", "model-c"}) {
		t.Fatalf("models = %v", session.Models)
	}
	if session.StartedAt == nil || !session.StartedAt.Equal(started) || session.StartedAt.Location() != time.UTC {
		t.Fatalf("start = %v, want earliest recorded turn in UTC", session.StartedAt)
	}
	if session.LastActivityAt == nil || !session.LastActivityAt.Equal(later) || session.LastActivityAt.Location() != time.UTC {
		t.Fatalf("activity = %v, want latest recorded turn in UTC", session.LastActivityAt)
	}
}

func TestHandoffRenderDecisions(t *testing.T) {
	t.Parallel()
	for status, want := range map[string]string{
		"COMPLETED": "[x]", "done": "[x]", "in-progress": "[ ] (in progress)", "active": "[ ] (in progress)", "unknown": "[ ]",
	} {
		box, suffix := planMarker(status)
		if got := box + suffix; got != want {
			t.Errorf("status %q = %q, want %q", status, got, want)
		}
	}
	h := Handoff{FullRecordPath: "/private/record.md", Gaps: []HandoffGap{{Code: "missing", Count: 2}}}
	if got := handoffFooterLines(h); len(got) != 1 || got[0] != "Capture gaps: missing ×2." {
		t.Fatalf("footer without elisions = %v", got)
	}
	h.Elisions = []HandoffElision{{Kind: HandoffElisionToolOutput, First: 1, Last: 1, Count: 1}}
	got := handoffFooterLines(h)
	if len(got) != 3 || !strings.Contains(got[1], "Full record: /private/record.md") || got[2] != "Capture gaps: missing ×2." {
		t.Fatalf("footer with elisions = %v", got)
	}
}
