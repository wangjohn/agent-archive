package cli

import (
	"strings"
	"testing"
)

// The detail screen's numbers table has the previous period's columns only
// when that period had something: a previous period of zeros (the first month
// of use) is no more than the absence of a change.
func TestStatsDetailComparesOnlyWithAPeriodThatHadSomething(t *testing.T) {
	t.Parallel()
	empty := realisticStats()
	empty.Overview.Sessions.Previous = f64(0)
	empty.Overview.ActiveDays.Previous = f64(0)
	if out := strings.Join(pageLines(pageDetail, empty, 80, false, false), "\n"); strings.Contains(out, "prior 30d") {
		t.Errorf("a previous period of zeros has columns:\n%s", out)
	}
	had := realisticStats()
	had.Overview.Sessions.Previous, had.Overview.Sessions.ChangePct = f64(60), f64(55)
	if out := strings.Join(pageLines(pageDetail, had, 80, false, false), "\n"); !strings.Contains(out, "prior 30d") {
		t.Errorf("a previous period with sessions has no columns:\n%s", out)
	}
}
