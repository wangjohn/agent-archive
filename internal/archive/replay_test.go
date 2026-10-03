package archive

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseReplayKeepsOnlyAnIdentifier(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]*Replay{
		"":                        nil,
		"   ":                     nil,
		"run-1":                   {RunID: "run-1"},
		" run-1 ":                 {RunID: "run-1"},
		"bench.2026-09-30:task_7": {RunID: "bench.2026-09-30:task_7"},
		strings.Repeat("a", 128):  {RunID: strings.Repeat("a", 128)},
		strings.Repeat("a", 129):  {},
		"1":                       {RunID: "1"},
		"-leading-dash":           {},
		"has space":               {},
		"fix the login bug":       {},
		"token=sk-synthetic":      {},
		"path/to/run":             {},
		"émoji":                   {},
		"true":                    {RunID: "true"},
	} {
		got := ParseReplay(value)
		if (got == nil) != (want == nil) || (got != nil && *got != *want) {
			t.Errorf("ParseReplay(%q) = %+v, want %+v", value, got, want)
		}
	}
}

func TestApplyReplayCopiesTheMarkerAndOnlyAnIdentifier(t *testing.T) {
	t.Parallel()
	var m Metadata
	m.ApplyReplay(SessionRegistration{})
	if m.IsReplay() {
		t.Fatal("an ordinary registration made a replay")
	}
	m.ApplyReplay(SessionRegistration{Replay: &Replay{RunID: "run-1"}})
	if encoded, _ := json.Marshal(m.Replay); string(encoded) != `{"run_id":"run-1"}` {
		t.Errorf("replay = %s", encoded)
	}
	// A registration edited by hand cannot put free text in the sidecar.
	m.ApplyReplay(SessionRegistration{Replay: &Replay{RunID: "fix the login bug"}})
	if encoded, _ := json.Marshal(m.Replay); !m.IsReplay() || string(encoded) != `{}` {
		t.Errorf("replay = %s, want the marker without the text", encoded)
	}
}
