package stats

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Day, week and month labels are checked against an oracle that uses only the
// time package's own calendar (Weekday, AddDate, Format), in zones with odd
// offsets (+5:45, +14, -11, a half-hour DST shift) and around their clock
// changes, for sessions placed on and just around every boundary.
func TestGroupLabelsMatchTheCalendarInAnyZone(t *testing.T) {
	t.Parallel()
	for _, zone := range []string{"Asia/Kathmandu", "Pacific/Kiritimati", "Pacific/Pago_Pago", "Australia/Lord_Howe", "America/St_Johns", "Europe/London", "America/New_York"} {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		rng := rand.New(rand.NewPCG(3, 3))
		nowT := time.Date(2026, time.November, 8, 0, 0, 0, 0, loc) // midnight exactly, days after several clock changes
		const days = 400
		var sessions []archive.Metadata
		add := func(at time.Time) {
			sessions = append(sessions, meta(fmt.Sprintf("s%d", len(sessions)), "claude", at, modelTokens("claude-opus-5-5", 1, 1, 0, 0)))
		}
		// Every local midnight in the window, one nanosecond either side, and noon.
		for d := range days + 5 {
			midnight := time.Date(2026, time.November, 8-d, 0, 0, 0, 0, loc)
			add(midnight)
			add(midnight.Add(-time.Nanosecond))
			add(midnight.Add(12 * time.Hour))
		}
		for range 500 {
			add(nowT.Add(-time.Duration(rng.Int64N(int64(days+2) * 24 * int64(time.Hour)))))
		}
		var got Stats
		for _, by := range []Grouping{GroupDay, GroupWeek, GroupMonth} {
			got = Compute(sessions, Options{Now: nowT, Location: loc, Days: days, By: by})
			want := map[string]int{}
			for _, m := range sessions {
				if m.CapturedAt.Before(got.Window.From) || !m.CapturedAt.Before(got.Window.To) {
					continue
				}
				local := m.CapturedAt.In(loc)
				y, mo, d := local.Date()
				date := time.Date(y, mo, d, 12, 0, 0, 0, time.UTC)
				switch by {
				case GroupDay:
				case GroupWeek:
					date = date.AddDate(0, 0, -((int(date.Weekday()) + 6) % 7))
				case GroupMonth:
					date = time.Date(y, mo, 1, 12, 0, 0, 0, time.UTC)
				case GroupNone, GroupProject:
				}
				key := date.Format("2006-01-02")
				if by == GroupMonth {
					key = date.Format("2006-01")
				}
				want[key]++
			}
			var keys []string
			for key := range want {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			if got.Groups == nil || len(got.Groups.Rows) != len(keys) {
				t.Fatalf("%s by %s: %d rows, want %d", zone, by, len(got.Groups.Rows), len(keys))
			}
			for i, row := range got.Groups.Rows {
				if row.Key != keys[i] || row.Sessions != want[keys[i]] {
					t.Fatalf("%s by %s: row %d = %s/%d, want %s/%d", zone, by, i, row.Key, row.Sessions, keys[i], want[keys[i]])
				}
			}
		}
	}
}
