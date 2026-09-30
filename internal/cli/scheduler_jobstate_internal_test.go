package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/scheduler/launchd"
)

// Characterization of the macOS scheduler (PR 5a-0), the part that names code
// a later PR moves or removes: launchd.ParseJobState (moved into the launchd
// adapter by 5a-2) and Env.Scheduler (the seam 5a-1 put in place of
// Env.JobState, LoadLaunchAgent and UnloadLaunchAgent), which give way to the
// port's own types. This file moves or changes with them, mechanically;
// scheduler_jobstate_test.go pins the same answers through status and names
// none of it.

// TestParseJobStateOverRecordedOutput reads the recordings of
// testdata/scheduler/launchctl-print with the parser itself.
func TestParseJobStateOverRecordedOutput(t *testing.T) {
	t.Parallel()
	plist := filepath.Join(t.TempDir(), "Library", "LaunchAgents", "com.agent-archive.collector.plist")
	read := func(file string) string {
		data, err := os.ReadFile(filepath.Join("testdata", "scheduler", "launchctl-print", file))
		must(t, err)
		return strings.ReplaceAll(string(data), "@PLIST@", plist)
	}
	for _, tc := range []struct {
		file   string
		failed error
		want   string
	}{
		{"running.txt", nil, "running"},
		{"loaded-not-running.txt", nil, "loaded"},
		{"missing.txt", errors.New("exit status 113"), "missing"},
		{"another-installation.txt", nil, "another_installation"},
		{"no-path-line.txt", nil, "unknown"},
		{"failed-unrelated.txt", errors.New("exit status 1"), "unknown"},
		// A print that succeeded but names no file is unknown, whatever
		// else it says; one that failed is unknown unless it says the
		// service is missing, even when it names this plist.
		{"missing.txt", nil, "unknown"},
		{"running.txt", errors.New("signal: killed"), "unknown"},
	} {
		if got := launchd.ParseJobState(read(tc.file), tc.failed, plist); string(got) != tc.want {
			t.Errorf("%s (error %v): %s, want %s", tc.file, tc.failed, got, tc.want)
		}
	}
}

// launchdAnswering is env asking launchd the way the program does, through
// launchctl, with run answering for launchctl: the Env's scheduler stand-in
// (which setupTestEnv sets) is cleared, so status reaches the code that runs
// launchctl print and reads its output.
func launchdAnswering(t *testing.T, env Env, run func(args ...string) ([]byte, error)) Env {
	t.Helper()
	env.Scheduler = nil
	stubLaunchctl(t, run)
	return env
}
