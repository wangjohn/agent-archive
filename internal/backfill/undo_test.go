package backfill

import (
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// The question names what happens to each session: deleted from the bucket,
// or only forgotten when its objects are in a previous destination.
func TestUndoQuestion(t *testing.T) {
	session := func(id, parent string, current bool) UndoSession {
		return UndoSession{Registration: archive.SessionRegistration{ArchiveSessionID: id, ParentSessionID: parent}, InCurrentDestination: current}
	}
	cases := []struct {
		name string
		plan UndoPlan
		want string
	}{
		{"deleted", UndoPlan{Sessions: []UndoSession{session("c", "a", true), session("a", "", true), session("b", "", true)}},
			"Delete 2 sessions from the archive? This cannot be undone."},
		{"forgotten", UndoPlan{Sessions: []UndoSession{session("a", "", false)}},
			"Forget 1 session? This cannot be undone."},
		{"mixed", UndoPlan{Sessions: []UndoSession{session("a", "", true), session("b", "", false), session("c", "", false)}},
			"Delete 1 session from the archive and forget 2 sessions from a previous destination? This cannot be undone."},
		{"subagents only", UndoPlan{Sessions: []UndoSession{session("c", "a", true), session("d", "a", true)}},
			"Delete 2 subagent transcripts from the archive? This cannot be undone."},
		{"projects only", UndoPlan{ExcludeProjects: []archive.ProjectActivation{{ProjectID: "p"}}},
			"Exclude 1 project?"},
	}
	for _, tc := range cases {
		if got := UndoQuestion(tc.plan); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
