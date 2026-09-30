package cli

import (
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Every view key switches to its view, drawn exactly as the static command
// draws it, with that view marked in the key bar.
func TestStatsScreenViewKeys(t *testing.T) {
	t.Parallel()
	run := runScreen(t, screenOptions{width: 100, height: 200}, "d", "p", "m", "a", "o", "q")
	if !run.ran || run.code != 0 || run.stderr != "" {
		t.Fatalf("ran=%v code=%d stderr=%q", run.ran, run.code, run.stderr)
	}
	pages := []statsPage{pageOverview, pageDetail, pageProjects, pageModels, pageAgents, pageOverview}
	if len(run.frames) != len(pages) {
		t.Fatalf("%d frames, want %d", len(run.frames), len(pages))
	}
	marks := map[statsPage]string{
		pageOverview: "[o overview]", pageDetail: "[d detail]", pageProjects: "[p projects]", pageModels: "[m models]", pageAgents: "[a agents]",
	}
	for i, page := range pages {
		lines := run.expectedLines(page, 30, 100)
		checkFrame(t, run.frames[i], lines, 0, 100, 200, barHas(marks[page], "w window 30d", "q quit"))
		if !strings.Contains(strings.Join(run.frames[i], "\n"), "agent-archive stats") {
			t.Errorf("frame %d has no title", i)
		}
	}
}

// Keys are not case sensitive, and the keys of another view do nothing to a
// terminal that is not asked: Enter, Backspace and the side arrows are ignored.
func TestStatsScreenIgnoresOtherKeys(t *testing.T) {
	t.Parallel()
	run := runScreen(t, screenOptions{width: 100, height: 200}, "D", "\r\x7f\x1b[C\x1b[D", "q")
	pages := []statsPage{pageOverview, pageDetail, pageDetail}
	if len(run.frames) != len(pages) {
		t.Fatalf("%d frames, want %d", len(run.frames), len(pages))
	}
	for i, page := range pages {
		checkFrame(t, run.frames[i], run.expectedLines(page, 30, 100), 0, 100, 200, anyBar)
	}
}

// w cycles 7, 30, 90 days and back, keeps the view, and recomputes the
// numbers for the new window: the same as the static command with that
// --days, and the title and the key bar say which.
func TestStatsScreenWindowCycles(t *testing.T) {
	t.Parallel()
	run := runScreen(t, screenOptions{width: 100, height: 200}, "d", "w", "w", "w", "q")
	windows := []int{30, 30, 90, 7, 30}
	pages := []statsPage{pageOverview, pageDetail, pageDetail, pageDetail, pageDetail}
	if len(run.frames) != len(windows) {
		t.Fatalf("%d frames", len(run.frames))
	}
	for i, days := range windows {
		bar := barHas("w window " + itoa(days) + "d")
		checkFrame(t, run.frames[i], run.expectedLines(pages[i], days, 100), 0, 100, 200, bar)
		if i > 0 && !strings.Contains(run.frames[i][0], "last "+itoa(days)+" days") {
			t.Errorf("frame %d title is %q, want last %d days", i, run.frames[i][0], days)
		}
	}
	if reflect.DeepEqual(run.frames[1], run.frames[2]) || reflect.DeepEqual(run.frames[2], run.frames[3]) {
		t.Error("changing the window changed nothing")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// A window --days or --since names that is not one of the usual ones is a
// custom entry among them, in order.
func TestStatsWindowCycle(t *testing.T) {
	t.Parallel()
	for start, want := range map[int]struct {
		windows []int
		index   int
	}{
		7:   {[]int{7, 30, 90}, 0},
		30:  {[]int{7, 30, 90}, 1},
		90:  {[]int{7, 30, 90}, 2},
		14:  {[]int{7, 14, 30, 90}, 1},
		1:   {[]int{1, 7, 30, 90}, 0},
		365: {[]int{7, 30, 90, 365}, 3},
	} {
		windows, index := statsWindowCycle(start)
		if !slices.Equal(windows, want.windows) || index != want.index {
			t.Errorf("statsWindowCycle(%d) = %v, %d; want %v, %d", start, windows, index, want.windows, want.index)
		}
	}
	// The usual windows are never changed by a custom one.
	statsWindowCycle(14)
	if !slices.Equal(statsWindowChoices, []int{7, 30, 90}) {
		t.Fatalf("statsWindowChoices = %v", statsWindowChoices)
	}
}

// A custom starting window cycles through itself.
func TestStatsScreenCustomWindow(t *testing.T) {
	t.Parallel()
	run := runScreen(t, screenOptions{width: 100, height: 200, days: 14}, "w", "w", "w", "w", "q")
	for i, days := range []int{14, 30, 90, 7, 14} {
		checkFrame(t, run.frames[i], run.expectedLines(pageOverview, days, 100), 0, 100, 200, barHas("w window "+itoa(days)+"d"))
	}
}

// The screen never draws more rows than the terminal has or wider than it,
// on the sizes people use, for every view, the help, and the save prompt.
func TestStatsScreenFitsTheTerminal(t *testing.T) {
	t.Parallel()
	for _, size := range []fixedTerminal{{80, 24}, {120, 40}, {50, 10}, {40, 8}, {200, 60}, {30, 5}, {20, 2}} {
		for _, color := range []bool{false, true} {
			run := runScreen(t, screenOptions{width: size.width, height: size.height, color: color},
				"d", "p", "m", "a", "o", "?", "\x1b[6~", "x", "h", "abc", "\x1b", "w", "\x1b[F", "q")
			if len(run.frames) < 12 {
				t.Fatalf("%v: %d frames", size, len(run.frames))
			}
			for i, rows := range run.frames {
				checkFrame(t, rows, currentLinesIgnored(rows, size.height), 0, size.width, size.height, anyBar)
				_ = i
			}
		}
	}
}

// currentLinesIgnored is the frame's own body, for checks that look only at
// the frame's shape.
func currentLinesIgnored(rows []string, height int) []string { return rows[:height-1] }
