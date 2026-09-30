package cli

import (
	"testing"
	"time"
)

// q and Ctrl-D quit at once: nothing after them is read, so no frame is drawn
// for it.
func TestStatsScreenQuitKeysStopReadingKeys(t *testing.T) {
	t.Parallel()
	for _, quit := range []string{"q", "Q", "\x04"} {
		run := runScreen(t, screenOptions{}, quit, "d", "d")
		if len(run.frames) != 1 || run.code != 0 {
			t.Errorf("%q: %d frames, code %d; want only the first screen", quit, len(run.frames), run.code)
		}
	}
}

// Esc does not quit: on a slow link an arrow key arrives in two reads, and the
// first is a lone Esc. The rest, arriving after the wait for it, is still that
// arrow key, so the screen scrolls instead of closing.
func TestStatsScreenEscDoesNotQuitAndASplitArrowScrolls(t *testing.T) {
	t.Parallel()
	const width, height = 60, 12
	run := runScreen(t, screenOptions{width: width, height: height}, "\x1b", later(300*time.Millisecond, "[B"), "q")
	if len(run.frames) != 3 {
		t.Fatalf("%d frames, want the first, the one after Esc and the one after the arrow", len(run.frames))
	}
	lines := run.expectedLines(pageOverview, 30, width)
	checkFrame(t, run.frames[1], lines, 0, width, height, anyBar)
	checkFrame(t, run.frames[2], lines, 1, width, height, anyBar)
}
