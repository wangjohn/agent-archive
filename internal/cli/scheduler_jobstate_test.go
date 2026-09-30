package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// Characterization of the macOS scheduler (PR 5a-0 of
// dev/proposals/platform-abstraction.md): how the CLI reads launchd's answer.
// testdata/scheduler/launchctl-print/ holds launchctl print output in the
// shapes launchd gives (its README says how they were made); the placeholder
// @PLIST@ stands for the plist launchd loaded the job from.

// printedJob is one recorded launchctl print, with the job state the CLI must
// read from it and whether launchctl exited with a failure.
type printedJob struct {
	file   string
	failed error
	want   string
}

var printedJobs = []printedJob{
	{"running.txt", nil, "running"},
	{"loaded-not-running.txt", nil, "loaded"},
	{"missing.txt", errors.New("exit status 113"), "missing"},
	{"another-installation.txt", nil, setupjournal.JobAnotherInstallation},
	{"no-path-line.txt", nil, "unknown"},
	{"failed-unrelated.txt", errors.New("exit status 1"), "unknown"},
}

func recordedPrint(t *testing.T, file, plist string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "scheduler", "launchctl-print", file))
	must(t, err)
	return strings.ReplaceAll(string(data), "@PLIST@", plist)
}

// status --json background has six values. Five come from launchctl's answer,
// and the sixth (broken) from the LaunchAgent's program, which launchd still
// calls loaded. status only asks (launchctl print, once) and never changes a
// job. The golden files record what else status says for each: its next
// action, and the row of the human screen.
//
// Not parallel: it replaces launchctl.
func TestStatusBackgroundForEveryJobState(t *testing.T) {
	project := t.TempDir()
	_, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, project))
	env.JobState, env.LoadLaunchAgent, env.UnloadLaunchAgent = nil, nil, nil
	home, _ := env.home()
	userHome, _ := env.userHomeDir()
	plist := env.installation(home, userHome).installedCollectorPlist()
	cfg, _, err := config.Load(home)
	must(t, err)

	// The last one runs with the program the LaunchAgent names gone.
	for _, job := range append(slices.Clone(printedJobs), printedJob{"running.txt", nil, backgroundBroken}) {
		name := strings.TrimSuffix(job.file, ".txt")
		if job.want == backgroundBroken {
			name = backgroundBroken
			must(t, os.Remove(cfg.InstalledExecutable))
		}
		var calls []string
		stubLaunchctl(t, func(args ...string) ([]byte, error) {
			calls = append(calls, strings.Join(args, " "))
			return []byte(recordedPrint(t, job.file, plist)), job.failed
		})
		decoded := statusJSON(t, env)
		if decoded["background"] != job.want {
			t.Errorf("%s: status --json background = %v, want %s", job.file, decoded["background"], job.want)
		}
		var human strings.Builder
		if code := runStatusCommand(nil, &human, os.Stderr, env); code != 0 {
			t.Fatalf("status exit %d", code)
		}
		if want := fmt.Sprintf("print gui/%d/%s", os.Getuid(), launchLabel(plist)); len(calls) != 2 || calls[0] != want || calls[1] != want {
			t.Errorf("%s: status ran launchctl %q, want two prints of %s", job.file, calls, want)
		}
		var rows []string
		for line := range strings.SplitSeq(human.String(), "\n") {
			if strings.Contains(line, "Background collector") {
				rows = append(rows, strings.TrimSpace(line))
			}
		}
		text := fmt.Sprintf("background: %v\nstate: %v\ncode: %v\nnext_action: %v\nscreen: %s\n", decoded["background"], decoded["state"], decoded["code"], decoded["next_action"], strings.Join(rows, " | "))
		text = strings.NewReplacer(local.CanonicalPath(project), "@PROJECT@", project, "@PROJECT@", plist, "@PLIST@", launchLabel(plist), "@LABEL@", userHome, "@USER_HOME@", cfg.InstalledExecutable, "@EXECUTABLE@").Replace(text)
		golden.Check(t, filepath.Join("testdata", "scheduler", "status", name+".txt"), []byte(text))
	}
}
