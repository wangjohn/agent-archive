package cli

import (
	"reflect"
	"strings"
	"testing"
)

// A view taller than the terminal scrolls by a line (arrows, j, k, and the
// wheel, which sends arrows), by a screen (PgUp, PgDn, space) and to its ends
// (Home, End); a burst of wheel arrows is one redraw; the top line never
// leaves the view; and the last row says where the screen is.
func TestStatsScreenScrolls(t *testing.T) {
	t.Parallel()
	const width, height = 50, 10
	steps := []struct {
		keys string
		top  int
	}{
		{"", 0},
		{"\x1b[B", 1},
		{"j", 2},
		{"\x1b[B\x1b[B\x1b[B", 5},
		{"\x1bOB", 6},
		{"\x1b[6~", 14},
		{"\x1b[5~", 6},
		{" ", 14},
		{"\x1b[F", -1},
		{"\x1b[6~", -1},
		{"k", -2},
		{"\x1b[H", 0},
		{"\x1b[A", 0},
		{"\x1b[5~", 0},
		{"K", 0},
	}
	var chunks []string
	for _, s := range steps[1:] {
		chunks = append(chunks, s.keys)
	}
	run := runScreen(t, screenOptions{width: width, height: height}, append(chunks, "q")...)
	lines := run.expectedLines(pageOverview, 30, width)
	maxTop := len(lines) - (height - 1)
	if maxTop < 20 {
		t.Fatalf("the overview is %d lines, too few to scroll at %d rows", len(lines), height)
	}
	if len(run.frames) != len(steps) {
		t.Fatalf("%d frames, want one per burst, %d", len(run.frames), len(steps))
	}
	for i, step := range steps {
		var top int
		switch step.top {
		case -1:
			top = maxTop
		case -2:
			top = maxTop - 1
		default:
			top = min(step.top, maxTop)
		}
		var bar func(string) bool
		switch top {
		case 0:
			bar = barHas("q quit", "Top")
		case maxTop:
			bar = barHas("q quit", "End")
		default:
			bar = barHas("q quit", "%")
		}
		checkFrame(t, run.frames[i], lines, top, width, height, bar)
	}
}

// A view that fits shows no position, and the key bar is the last row.
func TestStatsScreenShortViewShowsNoPosition(t *testing.T) {
	t.Parallel()
	run := runScreen(t, screenOptions{width: 100, height: 200}, "\x1b[B", "q")
	for _, rows := range run.frames {
		last := rows[len(rows)-1]
		if strings.HasSuffix(last, "Top") || strings.HasSuffix(last, "End") || strings.HasSuffix(last, "%") {
			t.Errorf("a view that fits shows a position: %q", last)
		}
	}
}

// The scroll position is kept across a window change (clamped to the new
// page) and reset by a view change.
func TestStatsScreenScrollAcrossViewsAndWindows(t *testing.T) {
	t.Parallel()
	const width, height = 60, 12
	run := runScreen(t, screenOptions{width: width, height: height}, "\x1b[6~", "w", "d", "q")
	lines := run.expectedLines(pageOverview, 30, width)
	checkFrame(t, run.frames[1], lines, min(height-2, len(lines)-(height-1)), width, height, anyBar)
	checkFrame(t, run.frames[3], run.expectedLines(pageDetail, 90, width), 0, width, height, anyBar)
}

// The size is read again for every draw: a resize redraws at the new size
// with the same view, and the scroll position stays within the new page.
func TestStatsScreenRedrawsOnResize(t *testing.T) {
	t.Parallel()
	size := &adjustableTerminal{size: fixedTerminal{80, 24}}
	fake := newFakeKeys("d", "\x1b[F", string(fakeResize), string(fakeResize), "q")
	resizes := 0
	fake.onResize = func() {
		resizes++
		if resizes == 1 {
			size.resize(fixedTerminal{60, 30})
		} else {
			size.resize(fixedTerminal{120, 80})
		}
	}
	run := runScreen(t, screenOptions{size: size, fake: fake})
	if len(run.frames) != 5 {
		t.Fatalf("%d frames", len(run.frames))
	}
	detail80 := run.expectedLines(pageDetail, 30, 80)
	checkFrame(t, run.frames[1], detail80, 0, 80, 24, anyBar)
	checkFrame(t, run.frames[2], detail80, len(detail80)-23, 80, 24, barHas("End"))
	detail60 := run.expectedLines(pageDetail, 30, 60)
	checkFrame(t, run.frames[3], detail60, min(len(detail80)-23, max(len(detail60)-29, 0)), 60, 30, anyBar)
	checkFrame(t, run.frames[4], run.expectedLines(pageDetail, 30, 120), 0, 120, 80, anyBar)
}

// ? opens the key list over the view; it lists every key; scrolling scrolls
// it; any other key closes it, back to the view where it was, and does
// nothing else (q there does not quit).
func TestStatsScreenHelp(t *testing.T) {
	t.Parallel()
	const width, height = 60, 12
	run := runScreen(t, screenOptions{width: width, height: height}, "\x1b[6~", "?", "\x1b[6~", "\x1b[H", "q", "q")
	if len(run.frames) != 6 {
		t.Fatalf("%d frames", len(run.frames))
	}
	page := run.expectedLines(pageOverview, 30, width)
	pageTop := min(height-2, len(page)-(height-1))
	checkFrame(t, run.frames[1], page, pageTop, width, height, anyBar)
	help := strings.Join(run.frames[2], "\n")
	if !strings.Contains(help, "keys") {
		t.Fatalf("no help:\n%s", help)
	}
	if reflect.DeepEqual(run.frames[2], run.frames[3]) {
		t.Error("PgDn did not scroll the help")
	}
	// The whole help, read at a tall terminal, names every key.
	all := runScreen(t, screenOptions{width: 100, height: 60}, "?", "x", "q")
	text := strings.Join(all.frames[1], "\n")
	for _, want := range []string{"overview", "detail", "projects", "models", "agents", "7d, 30d, 90d", "PgUp PgDn", "Home End", "save this window", "q Esc Ctrl-C", "--include-names"} {
		if !strings.Contains(text, want) {
			t.Errorf("the help lacks %q:\n%s", want, text)
		}
	}
	checkFrame(t, all.frames[2], all.expectedLines(pageOverview, 30, 100), 0, 100, 60, anyBar)
	// The frame that closed it is the view where it was; q there quit.
	checkFrame(t, run.frames[5], page, pageTop, width, height, anyBar)
	if run.fake.keyMode() {
		t.Error("key mode left on")
	}
}

// The key bar never wraps: at every width it is one row, marks the view,
// shows the window, and always offers q. Wide enough, it is whole.
func TestStatsKeyBarFitsEveryWidth(t *testing.T) {
	t.Parallel()
	for _, color := range []bool{false, true} {
		style := textStyle{color: color}
		for _, page := range statsPages {
			for width := 1; width <= 140; width++ {
				bar := statsKeyBar(style, page, 30, width)
				if w := visibleWidth(bar); w > width {
					t.Fatalf("width %d: the bar is %d columns: %q", width, w, bar)
				}
				if strings.Contains(bar, "\n") {
					t.Fatalf("width %d: the bar has a line break", width)
				}
				plain := ansiEscape.ReplaceAllString(bar, "")
				if width >= 6 && !strings.Contains(plain, "q") {
					t.Errorf("width %d: no q in %q", width, plain)
				}
				if width >= 60 && !strings.Contains(plain, "30d") {
					t.Errorf("width %d: no window in %q", width, plain)
				}
				if width >= 60 && !strings.Contains(plain, "? help") {
					t.Errorf("width %d: no help in %q", width, plain)
				}
			}
		}
	}
	whole := ansiEscape.ReplaceAllString(statsKeyBar(textStyle{}, pageDetail, 90, 200), "")
	if whole != " o overview  [d detail]  p projects  m models  a agents  w window 90d  h html  ? help  q quit" {
		t.Errorf("the full bar is %q", whole)
	}
	// The active view is in reverse video with color, and the bar is not
	// otherwise colored.
	colored := statsKeyBar(textStyle{color: true}, pageModels, 30, 200)
	if !strings.Contains(colored, "\x1b[7mm models\x1b[0m") || strings.Count(colored, "\x1b[7m") != 1 {
		t.Errorf("the active view is not in reverse video: %q", colored)
	}
}
