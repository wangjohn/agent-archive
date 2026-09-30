package stats

import (
	"encoding/json"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Whatever the zone (half-hour offsets, +14:00, daylight saving that skips
// midnight) and window length, the window is Days whole calendar days, the
// daily series has one entry per day, and a session at the first instant of
// the window is in while one just before it, or at its end, is out.
func TestWindowsInEveryZone(t *testing.T) {
	t.Parallel()
	zones := []string{"UTC", "Asia/Kolkata", "Pacific/Kiritimati", "America/Sao_Paulo", "Australia/Lord_Howe", "America/St_Johns", "Pacific/Apia", "Asia/Tehran"}
	for _, name := range zones {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Skipf("no zone data for %s: %v", name, err)
		}
		for _, days := range []int{-1, 0, 1, 2, 30, 400, MaxDays, MaxDays + 1000} {
			for _, at := range []time.Time{
				time.Date(2026, time.September, 29, 12, 0, 0, 0, loc),
				time.Date(2018, time.November, 3, 23, 30, 0, 0, loc), // Sao Paulo skipped the next midnight
				time.Date(2026, time.March, 8, 23, 59, 59, 0, loc),
				{},
			} {
				o := Options{Now: at, Location: loc, Days: days}
				got := Compute(hostileArchive(rand.New(rand.NewPCG(3, 3)), 60), o)
				want := days
				switch {
				case days <= 0:
					want = DefaultDays
				case days > MaxDays:
					want = MaxDays
				}
				if got.Window.Days != want || len(got.Daily) != want || got.Overview.DaysInWindow != want {
					t.Fatalf("%s days %d now %v: window %d, series %d", name, days, at, got.Window.Days, len(got.Daily))
				}
				if got.Daily[0].Date != got.Window.FirstDay || got.Daily[want-1].Date != got.Window.LastDay {
					t.Fatalf("%s: series %s..%s but window %s..%s", name, got.Daily[0].Date, got.Daily[want-1].Date, got.Window.FirstDay, got.Window.LastDay)
				}
				if _, err := json.Marshal(got); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if at.IsZero() {
					continue
				}
				edge := func(id string, when time.Time) archive.Metadata {
					return meta(id, "claude", when, modelTokens("claude-opus-5-5", 1, 0, 0, 0))
				}
				edges := Compute([]archive.Metadata{
					edge("first", got.Window.From), edge("last", got.Window.To.Add(-time.Second)),
					edge("after", got.Window.To), edge("before", got.Window.From.Add(-time.Nanosecond)),
				}, o)
				firstDay, lastDay := 1, 1
				if want == 1 {
					firstDay, lastDay = 2, 2
				}
				if edges.Coverage.Sessions != 2 || edges.Daily[0].Sessions != firstDay || edges.Daily[want-1].Sessions != lastDay {
					t.Errorf("%s days %d now %v: window %s..%s holds %d sessions (first day %d, last day %d), want the 2 inside it",
						name, days, at, got.Window.From, got.Window.To, edges.Coverage.Sessions, edges.Daily[0].Sessions, edges.Daily[want-1].Sessions)
				}
			}
		}
	}
}
