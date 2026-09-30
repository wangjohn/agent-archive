package stats

import (
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestStreaks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		active  string // one character per day, oldest first: x active, . not
		current int
		best    int
	}{
		{"nothing", "......", 0, 0},
		{"today only", ".....x", 1, 1},
		{"a streak that is still alive through yesterday", "..xxx.", 3, 3},
		{"broken two days ago", ".xxx..", 0, 3},
		{"best is earlier than current", "xxxx.xx", 2, 4},
		{"whole window", "xxxxx", 5, 5},
		{"single-day window, active", "x", 1, 1},
		{"single-day window, idle", ".", 0, 0},
		{"two-day window, only yesterday", "x.", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			active := make([]bool, len(tc.active))
			for i, c := range tc.active {
				active[i] = c == 'x'
			}
			current, best := streaks(active)
			if current != tc.current || best != tc.best {
				t.Fatalf("streaks(%s) = %d, %d; want %d, %d", tc.active, current, best, tc.current, tc.best)
			}
		})
	}
}

// session returns a one-token session captured at the given instant.
func session(id string, at time.Time) archive.Metadata {
	return meta(id, "claude", at, modelTokens("claude-opus-5-5", 1, 0, 0, 0))
}

func TestStreakAcrossDaylightSavingChanges(t *testing.T) {
	t.Parallel()
	loc := newYork
	if loc.String() != "America/New_York" {
		t.Skip("time zone database has no America/New_York")
	}
	for _, tc := range []struct {
		name     string
		now      time.Time
		days     int
		sessions []time.Time
		first    string
		streak   int
	}{
		{
			// Clocks fall back at 02:00 on Sunday Nov 1: that day has 25 hours.
			name: "fall back", now: time.Date(2026, time.November, 3, 12, 0, 0, 0, loc), days: 5,
			sessions: []time.Time{
				time.Date(2026, time.October, 31, 23, 30, 0, 0, loc),
				time.Date(2026, time.November, 1, 0, 30, 0, 0, loc),
				time.Date(2026, time.November, 1, 23, 30, 0, 0, loc),
				time.Date(2026, time.November, 2, 0, 15, 0, 0, loc),
				time.Date(2026, time.November, 3, 8, 0, 0, 0, loc),
			},
			first: "2026-10-30", streak: 4,
		},
		{
			// Clocks spring forward at 02:00 on Sunday Mar 8: that day has 23 hours.
			name: "spring forward", now: time.Date(2026, time.March, 10, 12, 0, 0, 0, loc), days: 5,
			sessions: []time.Time{
				time.Date(2026, time.March, 7, 23, 30, 0, 0, loc),
				time.Date(2026, time.March, 8, 12, 0, 0, 0, loc),
				time.Date(2026, time.March, 9, 0, 5, 0, 0, loc),
				time.Date(2026, time.March, 10, 8, 0, 0, 0, loc),
			},
			first: "2026-03-06", streak: 4,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var sessions []archive.Metadata
			for i, at := range tc.sessions {
				sessions = append(sessions, session(string(rune('a'+i)), at))
			}
			got := Compute(sessions, Options{Now: tc.now, Location: loc, Days: tc.days})
			if got.Overview.CurrentStreak != tc.streak || got.Overview.BestStreak != tc.streak {
				t.Fatalf("streak = %d (best %d), want %d; daily %+v", got.Overview.CurrentStreak, got.Overview.BestStreak, tc.streak, got.Daily)
			}
			if got.Window.FirstDay != tc.first {
				t.Fatalf("first day = %s, want %s", got.Window.FirstDay, tc.first)
			}
			// Every calendar day appears exactly once, in order, across the change.
			seen := map[string]bool{}
			for i, d := range got.Daily {
				if seen[d.Date] || (i > 0 && d.Date <= got.Daily[i-1].Date) {
					t.Fatalf("daily dates are not one per calendar day: %+v", got.Daily)
				}
				seen[d.Date] = true
			}
			if len(got.Daily) != tc.days {
				t.Fatalf("daily has %d days", len(got.Daily))
			}
			total := 0
			for _, d := range got.Daily {
				total += d.Sessions
			}
			if total != len(tc.sessions) {
				t.Fatalf("daily sessions = %d, want %d", total, len(tc.sessions))
			}
		})
	}
}

func TestTheSameInstantIsADifferentDayInAnotherZone(t *testing.T) {
	t.Parallel()
	tokyo := time.FixedZone("Asia/Tokyo", 9*60*60)
	utcMinus := time.FixedZone("UTC-8", -8*60*60)
	instant := time.Date(2026, time.September, 29, 2, 0, 0, 0, time.UTC)
	sessions := []archive.Metadata{session("a", instant)}
	for _, tc := range []struct {
		loc      *time.Location
		wantDay  string
		wantLast string
	}{
		{time.UTC, "2026-09-29", "2026-09-29"},
		{tokyo, "2026-09-29", "2026-09-29"},
		{utcMinus, "2026-09-28", "2026-09-28"},
	} {
		got := Compute(sessions, Options{Now: instant, Location: tc.loc, Days: 3})
		var active []string
		for _, d := range got.Daily {
			if d.Sessions > 0 {
				active = append(active, d.Date)
			}
		}
		if !slices.Equal(active, []string{tc.wantDay}) || got.Window.LastDay != tc.wantLast || got.Window.Timezone != tc.loc.String() {
			t.Errorf("%s: active days %v, window %+v", tc.loc, active, got.Window)
		}
	}
	// Half past midnight in Tokyo is the previous day in UTC: a window bound follows the zone.
	late := session("late", time.Date(2026, time.September, 28, 15, 30, 0, 0, time.UTC)) // Sep 29 00:30 in Tokyo
	in := Compute([]archive.Metadata{late}, Options{Now: instant, Location: tokyo, Days: 1})
	out := Compute([]archive.Metadata{late}, Options{Now: instant, Location: time.UTC, Days: 1})
	if in.Coverage.Sessions != 1 || out.Coverage.Sessions != 0 {
		t.Fatalf("tokyo %d, utc %d sessions", in.Coverage.Sessions, out.Coverage.Sessions)
	}
}

func TestWindowEdgesAreHalfOpen(t *testing.T) {
	t.Parallel()
	loc := newYork
	from := time.Date(2026, time.August, 31, 0, 0, 0, 0, loc)
	to := time.Date(2026, time.September, 30, 0, 0, 0, 0, loc)
	for _, tc := range []struct {
		name string
		at   time.Time
		in   bool
	}{
		{"one nanosecond before", from.Add(-time.Nanosecond), false},
		{"first instant", from, true},
		{"last instant", to.Add(-time.Nanosecond), true},
		{"the end is exclusive", to, false},
		{"a future capture is not counted", now.Add(48 * time.Hour), false},
	} {
		got := Compute([]archive.Metadata{session("a", tc.at)}, opts())
		if (got.Coverage.Sessions == 1) != tc.in {
			t.Errorf("%s: in window = %v, want %v", tc.name, got.Coverage.Sessions == 1, tc.in)
		}
	}
}

func TestGroupings(t *testing.T) {
	t.Parallel()
	if time.Date(2026, time.September, 21, 0, 0, 0, 0, time.UTC).Weekday() != time.Monday {
		t.Fatal("test assumes Sep 21 2026 is a Monday")
	}
	at := func(m time.Month, d int) time.Time { return day(m, d, 10) }
	sessions := []archive.Metadata{
		meta("a", "claude", at(time.August, 31), project("alpha"), turns(1), modelTokens("claude-opus-5-5", 10, 0, 0, 0)),
		meta("b", "claude", at(time.September, 1), project("alpha"), turns(2), modelTokens("claude-opus-5-5", 20, 0, 0, 0)),
		meta("c", "codex", at(time.September, 20), project("beta"), modelTokens("gpt-5", 100, 0, 0, 0)), // a Sunday
		meta("d", "claude", at(time.September, 21), project("alpha"), modelTokens("claude-opus-5-5", 5, 0, 0, 0)),
		meta("e", "cursor", at(time.September, 27), project("beta")), // a Sunday: the week of Sep 21
		meta("f", "claude", at(time.September, 28), project("alpha"), modelTokens("claude-opus-5-5", 1, 0, 0, 0)),
		meta("g", "claude", at(time.September, 28), project("alpha"), modelTokens("claude-opus-5-5", 1, 0, 0, 0)),
	}
	for _, tc := range []struct {
		by   Grouping
		want map[string]int // key -> sessions
		keys []string
	}{
		{GroupDay, map[string]int{"2026-08-31": 1, "2026-09-01": 1, "2026-09-20": 1, "2026-09-21": 1, "2026-09-27": 1, "2026-09-28": 2},
			[]string{"2026-08-31", "2026-09-01", "2026-09-20", "2026-09-21", "2026-09-27", "2026-09-28"}},
		{GroupWeek, map[string]int{"2026-08-31": 2, "2026-09-14": 1, "2026-09-21": 2, "2026-09-28": 2},
			[]string{"2026-08-31", "2026-09-14", "2026-09-21", "2026-09-28"}},
		{GroupMonth, map[string]int{"2026-08": 1, "2026-09": 6}, []string{"2026-08", "2026-09"}},
		{GroupProject, map[string]int{"alpha": 5, "beta": 2}, []string{"beta", "alpha"}}, // beta: 100 tokens, alpha: 37... beta first
	} {
		t.Run(string(tc.by), func(t *testing.T) {
			t.Parallel()
			got := Compute(sessions, Options{Now: now, Location: newYork, By: tc.by})
			if got.Groups == nil || got.Groups.By != tc.by {
				t.Fatalf("groups = %+v", got.Groups)
			}
			var keys []string
			total := 0
			for _, row := range got.Groups.Rows {
				keys = append(keys, row.Key)
				total += row.Sessions
				if row.Sessions != tc.want[row.Key] {
					t.Errorf("%s: %d sessions, want %d", row.Key, row.Sessions, tc.want[row.Key])
				}
			}
			if !slices.Equal(keys, tc.keys) || total != len(sessions) {
				t.Fatalf("keys = %v (%d sessions), want %v", keys, total, tc.keys)
			}
		})
	}
	// Without --by there are no groups; the Cursor session's tokens are unknown in its group.
	if g := Compute(sessions, opts()).Groups; g != nil {
		t.Fatalf("groups without By: %+v", g)
	}
	byProject := Compute(sessions, Options{Now: now, Location: newYork, By: GroupProject}).Groups.Rows
	if byProject[0].Key != "beta" || *byProject[0].Tokens != 100 || *byProject[1].Tokens != 37 || *byProject[1].Prompts != 3 || byProject[0].Prompts != nil {
		t.Fatalf("project rows = %+v", byProject)
	}
}

func TestParseGrouping(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"day", "week", "month", "project"} {
		if g, ok := ParseGrouping(s); !ok || string(g) != s {
			t.Errorf("ParseGrouping(%q) = %q, %v", s, g, ok)
		}
	}
	for _, s := range []string{"", "year", "Day"} {
		if _, ok := ParseGrouping(s); ok {
			t.Errorf("ParseGrouping(%q) accepted", s)
		}
	}
}

func TestWeekStartIsMonday(t *testing.T) {
	t.Parallel()
	for n := -20; n < 60; n++ {
		monday := weekStart(n)
		wd := time.Unix(int64(monday)*secondsPerDay, 0).UTC().Weekday()
		if wd != time.Monday || n-monday < 0 || n-monday > 6 {
			t.Fatalf("weekStart(%d) = %d (%s)", n, monday, wd)
		}
	}
}
