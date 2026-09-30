package cli

import "testing"

// q, Esc and Ctrl-D quit at once: nothing after them is read, so no frame is
// drawn for it.
func TestStatsScreenQuitKeysStopReadingKeys(t *testing.T) {
	t.Parallel()
	for _, quit := range []string{"q", "Q", "\x1b", "\x04"} {
		run := runScreen(t, screenOptions{}, quit, "d", "d")
		if len(run.frames) != 1 || run.code != 0 {
			t.Errorf("%q: %d frames, code %d; want only the first screen", quit, len(run.frames), run.code)
		}
	}
}
