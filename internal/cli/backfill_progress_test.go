package cli

import (
	"testing"

	"github.com/wangjohn/agent-archive/internal/backfill"
)

func TestPlanningDisplay(t *testing.T) {
	for _, tc := range []struct {
		name     string
		progress backfill.PlanningProgress
		want     string
	}{
		{"initial", backfill.PlanningProgress{}, "Looking for sessions…"},
		{"discovery", backfill.PlanningProgress{Stage: "Discovering sessions", Done: 123}, "Discovering sessions: 123 found"},
		{"starting", backfill.PlanningProgress{Stage: "Checking sessions", Total: 100}, "Checking sessions: 0/100 (0%)"},
		{"checking", backfill.PlanningProgress{Stage: "Checking sessions", Done: 25, Total: 100}, "Checking sessions: 25/100 (25%)"},
		{"complete", backfill.PlanningProgress{Stage: "Checking sessions", Done: 100, Total: 100}, "Checking sessions: 100/100 (100%)"},
		{"unknown total", backfill.PlanningProgress{Stage: "Checking native history"}, "Checking native history"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newPlanningDisplay("Looking for sessions…")
			d.observe(tc.progress)
			if got := d.label(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
