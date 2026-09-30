package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// Characterization of the macOS scheduler (PR 5a-0 of
// dev/proposals/platform-abstraction.md): what setup, uninstall and status ask
// launchctl, in what order, and what is on disk at each step. Later PRs move
// the launchctl calls behind a scheduler port; these tests observe only the
// launchctl seam (stubLaunchctl), the commands' output and the files, so they
// must pass unchanged through that move. A behavior change is a golden diff.
//
// schedRun is one installation on a fake Mac whose launchd is fakeLaunchd
// behind stubLaunchctl. It records every launchctl call with the state of the
// files that matter at that moment.
type schedRun struct {
	t        *testing.T
	env      Env
	home     string // the data directory
	userHome string
	account  string
	project  string
	fake     *fakeLaunchd
	// probes name files whose state is printed after each call: "absent", "old"
	// (bytes equal to the recorded ones), "new" (other bytes).
	probes  []schedProbe
	lines   []string
	answers map[string][]launchdAnswer // label -> what its next prints say, the last repeated
	failOne map[string]bool            // plists whose next bootstrap fails
	// failBootout are labels whose bootout always fails; crashAt is a plist
	// whose bootstrap ends the process (see interruptedSetup).
	failBootout map[string]bool
	crashAt     string
	exe         string
	uid         string
	stderr      string // what the last command wrote to standard error
}

type schedProbe struct {
	name string
	path string
	old  []byte
}

// newSchedRun is an installation of the account's default data directory (its
// label is the plain com.agent-archive.collector) or, when defaultInstall is
// false, of another one, whose label ends in a hash of its directory.
func newSchedRun(t *testing.T, defaultInstall bool) *schedRun {
	t.Helper()
	account, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	home := t.TempDir()
	if defaultInstall {
		home = filepath.Join(account, ".local", "share", "agent-archive")
	}
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	env.AccountHome = func() (string, error) { return account, nil }
	env.JobState, env.LoadLaunchAgent, env.UnloadLaunchAgent = nil, nil, nil
	exe, err := env.executable()
	must(t, err)
	r := &schedRun{t: t, env: env, home: home, userHome: userHome, account: account, project: project, exe: exe,
		fake: &fakeLaunchd{loaded: map[string]string{}}, answers: map[string][]launchdAnswer{}, failOne: map[string]bool{}, failBootout: map[string]bool{},
		uid: fmt.Sprintf("gui/%d", os.Getuid())}
	stubLaunchctl(t, r.launchctl)
	r.probe("journal", setupjournal.JournalPath(home), nil)
	return r
}

func (r *schedRun) own() string { return r.env.installation(r.home, r.userHome).collectorPlist() }

func (r *schedRun) agents(label string) string {
	return filepath.Join(r.userHome, "Library", "LaunchAgents", label+".plist")
}

func (r *schedRun) probe(name, path string, old []byte) {
	r.probes = append(r.probes, schedProbe{name, path, old})
}

func (r *schedRun) state() string {
	var parts []string
	for _, p := range r.probes {
		s := "absent"
		if data, err := os.ReadFile(p.path); err == nil {
			s = "new"
			if p.old == nil || bytes.Equal(data, p.old) {
				s = "present"
				if p.old != nil {
					s = "old"
				}
			}
		}
		parts = append(parts, p.name+"="+s)
	}
	return strings.Join(parts, " ")
}

// launchctl is the stand-in for launchctl: fakeLaunchd answers as launchd
// does, unless a test scripted the answers for a label's prints or made a
// bootstrap fail once.
func (r *schedRun) launchctl(args ...string) ([]byte, error) {
	r.lines = append(r.lines, strings.Join(args, " ")+"  |  "+r.state())
	switch args[0] {
	case "print":
		label := filepath.Base(args[1])
		if scripted := r.answers[label]; len(scripted) > 0 {
			state := scripted[0]
			if len(scripted) > 1 {
				r.answers[label] = scripted[1:]
			}
			return printAnswer(state, r.agents(label))
		}
	case "bootout":
		if r.failBootout[filepath.Base(args[1])] {
			return []byte("Boot-out failed: 5: Input/output error"), errors.New("exit status 5")
		}
	case "bootstrap":
		if args[2] == r.crashAt {
			panic(interruptedSetup{})
		}
		if r.failOne[args[2]] {
			delete(r.failOne, args[2])
			return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
		}
	}
	return r.fake.run(args...)
}

// launchdAnswer is what launchctl print says about a job: one of the job states
// the CLI tells apart.
type launchdAnswer string

const (
	answerLoaded  launchdAnswer = "loaded"
	answerMissing launchdAnswer = "missing"
	answerUnknown launchdAnswer = "unknown"
	// answerAnotherInstallation is a job launchd loaded from another plist.
	answerAnotherInstallation launchdAnswer = setupjournal.JobAnotherInstallation
)

// printAnswer is launchctl print for a job in one of those states, as a job
// loaded from plist would be described.
func printAnswer(answer launchdAnswer, plist string) ([]byte, error) {
	switch answer {
	case answerLoaded:
		return []byte("gui/501/x = {\n\tpath = " + plist + "\n\tstate = not running\n}\n"), nil
	case answerMissing:
		return []byte("Bad request.\nCould not find service \"x\" in domain for user gui: 501\n"), errors.New("exit status 113")
	case answerAnotherInstallation:
		return []byte("gui/501/x = {\n\tpath = /Users/real/Library/LaunchAgents/x.plist\n\tstate = running\n}\n"), nil
	case answerUnknown:
	}
	return []byte("launchctl could not answer\n"), errors.New("exit status 1")
}

// normalize replaces what varies between runs (temporary folders, the user id,
// the hash of a temporary data directory) with fixed names.
func (r *schedRun) normalize(s string) string {
	pairs := []string{r.userHome, "@USER_HOME@", r.home, "@DATA_HOME@", r.account, "@ACCOUNT@", r.project, "@PROJECT@", r.uid, "gui/UID"}
	if !r.env.installation(r.home, r.userHome).isDefault() {
		pairs = append(pairs, launchLabel(r.own()), "com.agent-archive.collector.@HASH@")
	}
	return strings.NewReplacer(append(pairs, r.exe, "@EXECUTABLE@")...).Replace(s)
}

// run is agent-archive args with no terminal input; it returns the exit code
// and everything the command wrote.
func (r *schedRun) run(args ...string) (int, string) {
	r.t.Helper()
	var out, errOut bytes.Buffer
	code := Run(args, strings.NewReader(""), &out, &errOut, r.env)
	r.stderr = errOut.String()
	return code, out.String() + errOut.String()
}

func (r *schedRun) setup(extra ...string) (int, string) {
	r.t.Helper()
	return r.run(append([]string{"setup", "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--region", "us-east-1", "--apps", "claude", "--project", r.project}, extra...)...)
}

// install runs an ordinary setup and forgets what launchctl was asked.
func (r *schedRun) install() {
	r.t.Helper()
	if code, out := r.setup(); code != 0 {
		r.t.Fatalf("setup: exit %d\n%s", code, out)
	}
	r.lines = nil
}

// earlierLabel writes the plist an earlier release left under label for this
// data directory; loaded says whether launchd runs it.
func (r *schedRun) earlierLabel(label string, loaded bool) {
	r.t.Helper()
	path := r.agents(label)
	plist, err := hooks.LaunchAgent("/opt/old/agent-archive", r.home, label, nil)
	must(r.t, err)
	must(r.t, local.WriteBytes(path, plist))
	if loaded {
		r.fake.loaded[label] = path
	}
	name := strings.TrimPrefix(label, hooks.LaunchLabel+".")
	if label == hooks.LaunchLabel {
		name = "default-label"
	}
	r.probe("earlier:"+name, path, nil)
}

// loadedPrototype writes the prototype's upload job, which the default
// installation retires, and has launchd run it.
func (r *schedRun) loadedPrototype() {
	r.t.Helper()
	path := r.agents(setupjournal.LegacyLaunchLabel)
	must(r.t, local.WriteBytes(path, []byte(legacyPlist)))
	r.fake.loaded[setupjournal.LegacyLaunchLabel] = path
	r.probe("prototype", path, nil)
}

// checkTranscript compares the calls and the end state with the golden file
// testdata/scheduler/transcripts/name.txt.
func (r *schedRun) checkTranscript(name, title string, code int) {
	r.t.Helper()
	got := fmt.Sprintf("# %s\n# each launchctl call, then the state of the files as it was asked\n%s\n# exit %d, end: %s\n",
		title, strings.Join(r.lines, "\n"), code, r.state())
	golden.Check(r.t, filepath.Join("testdata", "scheduler", "transcripts", name+".txt"), []byte(r.normalize(got)))
}

// Every launchctl call setup makes, in order, for an installation with nothing
// installed: the collector's state is asked (setup's first check, then the
// plan), and only then the new job is loaded. The journal exists from before
// the first change until after the last.
func TestFirstSetupLaunchctlSequence(t *testing.T) {
	for _, tc := range []struct {
		name           string
		defaultInstall bool
	}{{"first-setup-default", true}, {"first-setup-other-directory", false}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSchedRun(t, tc.defaultInstall)
			r.probe("own", r.own(), nil)
			code, out := r.setup()
			if code != 0 {
				t.Fatalf("setup: exit %d\n%s", code, out)
			}
			r.checkTranscript(tc.name, "first setup, nothing loaded", code)
		})
	}
}

// Setup over an installation whose job is loaded stops it first, writes the
// files (the plist changes from old to new), and starts the new job.
func TestResetupLaunchctlSequence(t *testing.T) {
	r := newSchedRun(t, true)
	r.install()
	old, err := os.ReadFile(r.own())
	must(t, err)
	other := testExecutable(t)
	r.env.Executable = func() (string, error) { return other, nil }
	r.probe("own", r.own(), old)
	code, out := r.setup()
	if code != 0 {
		t.Fatalf("setup: exit %d\n%s", code, out)
	}
	if data, _ := os.ReadFile(r.own()); bytes.Equal(data, old) {
		t.Fatal("the plist was not rewritten for the new executable")
	}
	r.checkTranscript("resetup-loaded", "setup again from another executable, job loaded", code)
}

// The collectors earlier releases left for this data directory are stopped
// and their plists removed after the new files are written and before the new
// job is loaded; one that launchd does not run is only removed.
func TestSetupRetiresEarlierLabelsBeforeLoading(t *testing.T) {
	r := newSchedRun(t, true)
	r.probe("own", r.own(), nil)
	r.earlierLabel(hooks.CollectorLabel("/old/spelling/a", ""), true)
	r.earlierLabel(hooks.CollectorLabel("/old/spelling/b", ""), false)
	code, out := r.setup()
	if code != 0 {
		t.Fatalf("setup: exit %d\n%s", code, out)
	}
	r.checkTranscript("setup-retires-earlier-labels", "setup with two earlier-label collectors, one loaded", code)
}

// An installation in another data directory retires the collector that releases
// before labels were per directory installed under the default label.
func TestSetupMovesAJobOffTheDefaultLabelLaunchctlOrder(t *testing.T) {
	r := newSchedRun(t, false)
	r.probe("own", r.own(), nil)
	r.earlierLabel(hooks.LaunchLabel, true)
	code, out := r.setup()
	if code != 0 {
		t.Fatalf("setup: exit %d\n%s", code, out)
	}
	r.checkTranscript("setup-moves-job-off-default-label", "setup in another directory, its collector loaded under the default label", code)
}

// The default installation retires the prototype's upload job the same way.
func TestSetupMigratesThePrototypeJobBeforeLoading(t *testing.T) {
	r := newSchedRun(t, true)
	r.probe("own", r.own(), nil)
	r.loadedPrototype()
	code, out := r.setup()
	if code != 0 {
		t.Fatalf("setup: exit %d\n%s", code, out)
	}
	r.checkTranscript("setup-migrates-prototype", "setup with the prototype's upload job loaded", code)
}

// The whole history at once: this installation's job, the prototype's, and
// two earlier-label collectors all loaded. The order is the contract: stop own,
// write files, retire the prototype, retire the earlier labels, load own.
func TestSetupWithEveryKindOfOldJobLaunchctlOrder(t *testing.T) {
	r := newSchedRun(t, true)
	r.install()
	old, err := os.ReadFile(r.own())
	must(t, err)
	other := testExecutable(t)
	r.env.Executable = func() (string, error) { return other, nil }
	r.probe("own", r.own(), old)
	r.loadedPrototype()
	r.earlierLabel(hooks.CollectorLabel("/old/spelling/a", ""), true)
	r.earlierLabel(hooks.CollectorLabel("/old/spelling/b", ""), true)
	code, out := r.setup()
	if code != 0 {
		t.Fatalf("setup: exit %d\n%s", code, out)
	}
	r.checkTranscript("setup-every-kind-of-old-job", "setup with own, prototype and two earlier-label jobs loaded", code)
}

// A new job that fails to start puts everything back: the old files, and the
// jobs setup stopped, restarted in the order they were stopped.
func TestFailedStartRollsBackLaunchctlSequence(t *testing.T) {
	r := newSchedRun(t, true)
	r.install()
	old, err := os.ReadFile(r.own())
	must(t, err)
	other := testExecutable(t)
	r.env.Executable = func() (string, error) { return other, nil }
	r.probe("own", r.own(), old)
	r.loadedPrototype()
	r.earlierLabel(hooks.CollectorLabel("/old/spelling/a", ""), true)
	r.failOne[r.own()] = true
	code, out := r.setup()
	if code == 0 {
		t.Fatalf("setup succeeded although the job could not start:\n%s", out)
	}
	r.checkTranscript("setup-start-fails-rollback", "setup whose new job fails to start", code)
}

// Uninstall stops this installation's job and every earlier-label collector
// of the directory that launchd runs, by service target, then removes their
// plists.
func TestUninstallLaunchctlSequence(t *testing.T) {
	r := newSchedRun(t, true)
	r.install()
	r.probe("own", r.own(), nil)
	r.earlierLabel(hooks.CollectorLabel("/old/spelling/a", ""), true)
	r.earlierLabel(hooks.CollectorLabel("/old/spelling/b", ""), false)
	code, out := r.run("uninstall", "--yes")
	if code != 0 {
		t.Fatalf("uninstall: exit %d\n%s", code, out)
	}
	r.checkTranscript("uninstall-loaded", "uninstall with own and two earlier-label collectors, two loaded", code)
}
