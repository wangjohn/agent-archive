package cli

import (
	"path/filepath"
	"testing"
)

// Characterization of the macOS scheduler (PR 5a-0): parseJobState over the
// recorded launchctl print output of scheduler_jobstate_test.go. It calls the
// parser directly, and moves with it (into the launchd adapter) in a later PR;
// TestStatusBackgroundForEveryJobState reads the same files through status.
func TestParseJobStateOverRecordedOutput(t *testing.T) {
	t.Parallel()
	plist := filepath.Join(t.TempDir(), "Library", "LaunchAgents", "com.agent-archive.collector.plist")
	for _, job := range printedJobs {
		if got := parseJobState(recordedPrint(t, job.file, plist), job.failed, plist); got != job.want {
			t.Errorf("%s: %s, want %s", job.file, got, job.want)
		}
	}
	// A failed print that still names the service missing is missing, and one
	// that succeeded but describes no file is unknown, whatever else it says.
	if got := parseJobState(recordedPrint(t, "missing.txt", plist), nil, plist); got != "unknown" {
		t.Errorf("a successful print of a missing service: %s, want unknown", got)
	}
}
