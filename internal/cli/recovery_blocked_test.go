package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

// U-22: when launchctl fails while an interrupted setup is being recovered,
// rerunning setup alone stops at the same place, so setup advertises
// --abandon-recovery, as it does for a file changed outside setup. The same
// holds for a recovery record that cannot be read at all, which
// --abandon-recovery then moves aside.
func TestRecoveryBlockedByLaunchctlAdvertisesAbandon(t *testing.T) {
	for _, failure := range []string{"restart", "stop"} {
		t.Run(failure, func(t *testing.T) {
			home, userHome := t.TempDir(), t.TempDir()
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
			plist := env.installation(home, userHome).collectorPlist()
			must(t, local.Write(journalPath(home), setupJournal{Plist: plist, WasLoaded: true}))
			state := "missing"
			if failure == "stop" {
				state = "loaded"
			}
			env.JobState = func(string) string { return state }
			env.LoadLaunchAgent = func(string) error { return errors.New("Bootstrap failed: 5: Input/output error") }
			env.UnloadLaunchAgent = func(string) error { return errors.New("Boot-out failed: 5: Input/output error") }
			output := setupRun(t, env, "", 1)
			if !strings.Contains(output, "agent-archive setup --abandon-recovery") || !strings.Contains(output, "launchctl could not") || !strings.Contains(output, journalPath(home)) {
				t.Fatalf("output:\n%s", output)
			}
		})
	}
}

func TestUnreadableRecoveryRecordHasAWayOut(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	must(t, os.WriteFile(journalPath(home), []byte(`{"changes":[`), 0o600))
	output := setupRun(t, env, "", 1)
	if !strings.Contains(output, "--abandon-recovery") || !strings.Contains(output, journalPath(home)) {
		t.Fatalf("setup:\n%s", output)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "--abandon-recovery"}, nil, &out, &errOut, env); code != 0 || !strings.Contains(out.String(), "moved to") {
		t.Fatalf("abandon: exit %d\n%s%s", code, &out, &errOut)
	}
	if transactionPending(home) {
		t.Fatal("the record is still there")
	}
	if aside, _ := filepath.Glob(journalPath(home) + ".*" + movedAsideSuffix); len(aside) != 1 {
		t.Fatalf("moved aside: %v", aside)
	}
}
