package stats

import "time"

// civil is a calendar date, without a time zone.
type civil struct {
	year  int
	month time.Month
	day   int
}

func civilOf(t time.Time) civil {
	year, month, day := t.Date()
	return civil{year, month, day}
}

const secondsPerDay = 24 * 60 * 60

// dayNumber counts the days from 1970-01-01 to a calendar date. Days are
// counted on the calendar, never by adding 24 hours, so a day that is 23 or
// 25 hours long (daylight-saving change) is still one day.
func dayNumber(c civil) int {
	return int(time.Date(c.year, c.month, c.day, 0, 0, 0, 0, time.UTC).Unix() / secondsPerDay)
}

func civilFromDayNumber(n int) civil {
	return civilOf(time.Unix(int64(n)*secondsPerDay, 0).UTC())
}

func dateString(n int) string {
	return time.Unix(int64(n)*secondsPerDay, 0).UTC().Format("2006-01-02")
}

// startOfDay is the first instant of day number n in loc: midnight, or, in a
// zone whose clocks skip midnight (São Paulo before 2019 sprang forward from
// 00:00 to 01:00), the first moment that exists on that date. time.Date alone
// would return an instant on the day before there.
func startOfDay(n int, loc *time.Location) time.Time {
	c := civilFromDayNumber(n)
	t := time.Date(c.year, c.month, c.day, 0, 0, 0, 0, loc)
	for dayNumber(civilOf(t)) < n {
		t = t.Add(time.Minute)
	}
	return t
}

// floorMod is a modulo that is never negative.
func floorMod(a, n int) int {
	return ((a % n) + n) % n
}

// weekStart is the day number of the Monday on or before day n. Day 0
// (1970-01-01) was a Thursday.
func weekStart(n int) int {
	const thursdayOffset = 3 // days from Monday to 1970-01-01
	return n - floorMod(n+thursdayOffset, 7)
}

// streaks returns the run of active days ending today (or yesterday when
// today is not active yet) and the longest run, over days 0..len-1.
func streaks(active []bool) (current, best int) {
	run := 0
	for _, on := range active {
		if on {
			run++
			best = max(best, run)
		} else {
			run = 0
		}
	}
	end := len(active) - 1
	if end >= 1 && !active[end] {
		end--
	}
	for i := end; i >= 0 && active[i]; i-- {
		current++
	}
	return current, best
}
