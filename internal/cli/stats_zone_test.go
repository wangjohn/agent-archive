package cli

import (
	"testing"
	"time"
)

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("zone %s: %v", name, err)
	}
	return loc
}

// The machine's zone is named from $TZ, or from where /etc/localtime points
// when $TZ is unset, and only when the name is the same zone: a name that
// merely shares today's offset, an unresolvable $TZ, or a link that is not a
// zone file all leave the clock's own zone, never a guess.
func TestResolveLocalZoneNamesTheZoneOnlyWhenSure(t *testing.T) {
	t.Parallel()
	la := mustZone(t, "America/Los_Angeles")
	utc := time.UTC
	london := mustZone(t, "Europe/London")
	winter := time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC)
	summer := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		local     *time.Location
		now       time.Time
		tz        string
		tzSet     bool
		link      string
		want      string
		wantLocal bool
	}{
		{name: "TZ names it", local: la, now: summer, tz: "America/Los_Angeles", tzSet: true, want: "America/Los_Angeles"},
		{name: "TZ with a colon", local: la, now: summer, tz: ":America/Los_Angeles", tzSet: true, want: "America/Los_Angeles"},
		{name: "TZ as a zone file path", local: la, now: summer, tz: "/usr/share/zoneinfo/America/Los_Angeles", tzSet: true, want: "America/Los_Angeles"},
		{name: "link when TZ is unset", local: la, now: summer, link: "/var/db/timezone/zoneinfo/America/Los_Angeles", want: "America/Los_Angeles"},
		{name: "link relative", local: la, now: summer, link: "../usr/share/zoneinfo/America/Los_Angeles", want: "America/Los_Angeles"},
		// TZ=UTC0 does not name a zone file: Go counts in UTC, and a
		// London link must not be believed just because January's offset is
		// the same.
		{name: "TZ=UTC0 in winter is not London", local: utc, now: winter, tz: "UTC0", tzSet: true, link: "/usr/share/zoneinfo/Europe/London", wantLocal: true},
		// An empty TZ is UTC to Go, whatever /etc/localtime says.
		{name: "empty TZ is not the link", local: utc, now: winter, tz: "", tzSet: true, link: "/usr/share/zoneinfo/Europe/London", wantLocal: true},
		{name: "TZ that is another zone with today's offset", local: london, now: winter, tz: "Africa/Abidjan", tzSet: true, wantLocal: true},
		{name: "no TZ, no link", local: la, now: summer, wantLocal: true},
		{name: "link is not a zone file", local: la, now: summer, link: "/etc/somewhere/else", wantLocal: true},
		{name: "link to a missing zone", local: la, now: summer, link: "/usr/share/zoneinfo/Nowhere/Land", wantLocal: true},
		{name: "link that says Local", local: la, now: summer, link: "/usr/share/zoneinfo/Local", wantLocal: true},
		{name: "traversal in TZ", local: la, now: summer, tz: "../../etc/passwd", tzSet: true, wantLocal: true},
		{name: "escape in TZ", local: la, now: summer, tz: "America/Los_Angeles\x1b[2J", tzSet: true, wantLocal: true},
		{name: "wrong zone in TZ", local: la, now: summer, tz: "Asia/Tokyo", tzSet: true, wantLocal: true},
		// Same offset today, different rules: not the same zone.
		{name: "same offset in winter, different rules", local: la, now: winter, tz: "America/Phoenix", tzSet: true, wantLocal: true},
	} {
		got := resolveLocalZone(tc.local, tc.now, tc.tz, tc.tzSet, tc.link)
		switch {
		case tc.wantLocal && got != tc.local:
			t.Errorf("%s: got %v, want the clock's own zone", tc.name, got)
		case !tc.wantLocal && got.String() != tc.want:
			t.Errorf("%s: got %v, want %s", tc.name, got, tc.want)
		}
	}
}
