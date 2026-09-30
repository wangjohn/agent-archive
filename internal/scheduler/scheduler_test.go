package scheduler

import (
	"testing"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

// The states are the words `status --json` reports as `background` and the
// setup journal records, so their values are part of the tool's output:
// changing one is a format change, not a rename.
func TestJobStatesAreWhatStatusReports(t *testing.T) {
	t.Parallel()
	for state, want := range map[JobState]string{
		Loaded:              "loaded",
		Running:             "running",
		Missing:             "missing",
		Unknown:             "unknown",
		AnotherInstallation: "another_installation",
	} {
		if string(state) != want {
			t.Errorf("state %q, want %q", state, want)
		}
	}
}
