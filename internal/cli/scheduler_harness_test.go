package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/scheduler/launchd"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

// The harness of the macOS scheduler characterization (PR 5a-0 of
// dev/proposals/platform-abstraction.md; the tests are the other
// scheduler_*_test.go files). Later PRs move the launchctl calls behind a
// scheduler port and the launchd history into its adapter; the tests must
// pass through that without their assertions changing. So the tests observe
// only public behavior: the launchctl argument vectors (through the
// launchctl Runner seam, stubLaunchctl), the files on disk, the journal as JSON,
// and what the commands print. This file is the only one of them that names
// the code under test's internals (installation, hooks' labels and plists,
// setupjournal; Env's launchd fields through launchdAnswering, which the
// status characterization shares); a PR that moves those edits this file
// (or launchdAnswering) and no other.
//
// schedRun is one installation on a fake Mac whose launchd is modeled here:
// it records every launchctl call with the state of the files that matter at
// that moment.
type schedRun struct {
	t         *testing.T
	env       Env
	home      string // the data directory
	userHome  string
	account   string
	project   string
	isDefault bool
	exe       string
	uid       string
	// loaded is launchd's state: label -> the plist it loaded the label from.
	loaded map[string]string
	// probes name files whose state is printed after each call: "absent",
	// "present", or, for a probe with recorded bytes, "old" (equal to them)
	// or "new" (other bytes).
	probes  []schedProbe
	lines   []string
	answers map[string][]launchdAnswer // label -> what its next prints say, the last repeated
	failOne map[string]bool            // plists whose next bootstrap fails
	// failBootout are labels whose bootout always fails; crashAt is a plist
	// whose bootstrap ends the process (see interruptedSetup).
	failBootout map[string]bool
	crashAt     string
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
	exe, err := env.executable()
	must(t, err)
	r := &schedRun{t: t, env: env, home: home, userHome: userHome, account: account, project: project, isDefault: defaultInstall, exe: exe,
		loaded: map[string]string{}, answers: map[string][]launchdAnswer{}, failOne: map[string]bool{}, failBootout: map[string]bool{},
		uid: fmt.Sprintf("gui/%d", os.Getuid())}
	// launchd is reached only through launchctl, which r.launchctl models;
	// launchdAnswering (scheduler_jobstate_internal_test.go) is the one place
	// the tests clear the Env's scheduler stand-ins.
	r.env = launchdAnswering(t, env, r.launchctl)
	r.probe("journal", r.journalPath(), nil)
	return r
}

// own is this installation's collector plist; ownLabel is its label.
func (r *schedRun) own() string { return r.env.installation(r.home, r.userHome).collectorPlist() }

func (r *schedRun) ownLabel() string { return plistLabel(r.own()) }

// journalPath is where setup records its transaction; journalPending reports
// whether that record is there.
func (r *schedRun) journalPath() string { return setupjournal.JournalPath(r.home) }

func (r *schedRun) journalPending() bool {
	_, err := os.Stat(r.journalPath())
	return !os.IsNotExist(err)
}

// useNewExecutable has later commands run from another executable, as after
// an upgrade that moved the binary, and returns its path.
func (r *schedRun) useNewExecutable() string {
	r.t.Helper()
	other := testExecutable(r.t)
	r.env.Executable = func() (string, error) { return other, nil }
	return other
}

// hooksInstalled reports whether Claude Code's settings hold this tool's hooks.
func (r *schedRun) hooksInstalled() bool {
	data, _ := os.ReadFile(filepath.Join(r.userHome, ".claude", "settings.json"))
	return bytes.Contains(data, []byte(hooks.Owner))
}

// earlierLabel is the label an earlier release gave this data directory when
// it was spelled spelling (releases before labels were canonical).
func earlierLabel(spelling string) string { return launchd.CollectorLabel(spelling, "") }

// defaultLabel is the label every installation had before labels were per
// directory, and the default installation's still.
const defaultLabel = launchd.LaunchLabel

// prototypeLabel is the prototype's upload job, which the default
// installation retires.
const prototypeLabel = setupjournal.LegacyLaunchLabel

// earlierCollector writes the plist an earlier release left under label for
// this data directory; loaded says whether launchd runs it.
func (r *schedRun) earlierCollector(label string, loaded bool) {
	r.t.Helper()
	path := r.agents(label)
	plist, err := launchd.LaunchAgent("/opt/old/agent-archive", r.home, label, nil)
	must(r.t, err)
	writeFile(r.t, path, plist)
	if loaded {
		r.loaded[label] = path
	}
	name := strings.TrimPrefix(label, defaultLabel+".")
	if label == defaultLabel {
		name = "default-label"
	}
	r.probe("earlier:"+name, path, nil)
}

// prototypePlist is the prototype's upload job as it installed it.
const prototypePlist = `<?xml version="1.0"?><plist><dict><key>Label</key><string>com.agent-skills.skill-runs-upload</string><key>ProgramArguments</key><array><string>/usr/bin/python3</string><string>/private/runtime/skill_runs.py</string><string>--home</string><string>/private/records</string><string>upload</string></array></dict></plist>`

// loadedPrototype writes the prototype's upload job and has launchd run it.
func (r *schedRun) loadedPrototype() {
	r.t.Helper()
	path := r.agents(prototypeLabel)
	writeFile(r.t, path, []byte(prototypePlist))
	r.loaded[prototypeLabel] = path
	r.probe("prototype", path, nil)
}

// normalize replaces what varies between runs (temporary folders, the user id,
// the hash of a temporary data directory) with fixed names.
func (r *schedRun) normalize(s string) string {
	pairs := []string{r.userHome, "@USER_HOME@", r.home, "@DATA_HOME@", r.account, "@ACCOUNT@", r.project, "@PROJECT@", r.uid, "gui/UID"}
	if !r.isDefault {
		pairs = append(pairs, r.ownLabel(), defaultLabel+".@HASH@")
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

// setup is setup --yes with every answer given.
func (r *schedRun) setup(extra ...string) (int, string) {
	r.t.Helper()
	return r.run(append([]string{"setup", "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--region", "us-east-1", "--apps", "claude", "--project", r.project}, extra...)...)
}

// ---- The fake Mac. Nothing below names the code under test. ----

// plistLabel is the label of a job this tool installs: its plist's file name.
func plistLabel(plist string) string { return strings.TrimSuffix(filepath.Base(plist), ".plist") }

func (r *schedRun) agents(label string) string {
	return filepath.Join(r.userHome, "Library", "LaunchAgents", label+".plist")
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(path, data, 0o600))
}

func (r *schedRun) probe(name, path string, old []byte) {
	r.probes = append(r.probes, schedProbe{name, path, old})
}

func (r *schedRun) state() string {
	var parts []string
	for _, p := range r.probes {
		s := "absent"
		if data, err := os.ReadFile(p.path); err == nil {
			switch {
			case p.old == nil:
				s = "present"
			case bytes.Equal(data, p.old):
				s = "old"
			default:
				s = "new"
			}
		}
		parts = append(parts, p.name+"="+s)
	}
	return strings.Join(parts, " ")
}

// running reports whether launchd runs the job plist defines, from plist.
func (r *schedRun) running(plist string) bool { return r.loaded[plistLabel(plist)] == plist }

// launchctl is the stand-in for launchctl. It answers as launchd does for the
// three calls the CLI makes (print gui/UID/label, bootout gui/UID/label,
// bootstrap gui/UID plist), unless a test scripted what a label's prints say,
// made a bootstrap fail once, or made a bootout fail. Like launchd, it refuses
// to bootstrap a label it already runs, from any plist: a test cannot record
// a restart that a Mac would not do.
//
// Its failure texts are the ones the rest of this package's tests use
// ("Bootstrap failed: 5: Input/output error"), not a recording: the real
// launchctl ends them with a newline (and may add a line), and its code and
// words differ by cause (a bootout of a label it does not run is not an
// input/output error). The goldens that quote them pin how the CLI wraps
// what launchctl said, not launchctl's own words; had the stand-in kept the
// newline, a later PR that trims launchctl's output would change them for a
// reason that is not the CLI's behavior.
func (r *schedRun) launchctl(args ...string) ([]byte, error) {
	r.lines = append(r.lines, strings.Join(args, " ")+"  |  "+r.state())
	switch {
	case len(args) == 2 && args[0] == "print":
		label := path.Base(args[1])
		if scripted := r.answers[label]; len(scripted) > 0 {
			if len(scripted) > 1 {
				r.answers[label] = scripted[1:]
			}
			return printAnswer(scripted[0], label, r.agents(label))
		}
		if from, ok := r.loaded[label]; ok {
			return printAnswer(answerLoaded, label, from)
		}
		return printAnswer(answerMissing, label, "")
	case len(args) == 2 && args[0] == "bootout":
		label := path.Base(args[1])
		if _, ok := r.loaded[label]; !ok || r.failBootout[label] {
			return []byte("Boot-out failed: 5: Input/output error"), errors.New("exit status 5")
		}
		delete(r.loaded, label)
		return nil, nil
	case len(args) == 3 && args[0] == "bootstrap":
		plist := args[2]
		if plist == r.crashAt {
			panic(interruptedSetup{})
		}
		label := plistLabel(plist)
		_, statErr := os.Stat(plist)
		if r.failOne[plist] || r.labelTaken(label) || statErr != nil {
			delete(r.failOne, plist)
			return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
		}
		r.loaded[label] = plist
		return nil, nil
	}
	r.t.Fatalf("unexpected launchctl call %q", args)
	return nil, nil
}

// labelTaken reports whether launchd runs label, as the test scripted its
// prints or, when it scripted none or only that launchctl cannot answer
// (which says nothing of what launchd runs), as the fake loaded it.
func (r *schedRun) labelTaken(label string) bool {
	if scripted := r.answers[label]; len(scripted) > 0 && scripted[0] != answerUnknown {
		return scripted[0] == answerLoaded || scripted[0] == answerAnotherInstallation
	}
	_, ok := r.loaded[label]
	return ok
}

// launchdAnswer is what launchctl print says about a job: one of the job states
// the CLI tells apart.
type launchdAnswer string

const (
	answerLoaded  launchdAnswer = "loaded"
	answerMissing launchdAnswer = "missing"
	answerUnknown launchdAnswer = "unknown"
	// answerAnotherInstallation is a job launchd loaded from another plist.
	answerAnotherInstallation launchdAnswer = "another_installation"
)

// printAnswer is launchctl print for a job in one of those states; a loaded
// job was loaded from plist.
func printAnswer(answer launchdAnswer, label, plist string) ([]byte, error) {
	switch answer {
	case answerLoaded:
		return []byte("gui/501/" + label + " = {\n\tpath = " + plist + "\n\tstate = running\n}\n"), nil
	case answerMissing:
		return []byte("Bad request.\nCould not find service \"" + label + "\" in domain for user gui: 501\n"), errors.New("exit status 113")
	case answerAnotherInstallation:
		return []byte("gui/501/" + label + " = {\n\tpath = /Users/real/Library/LaunchAgents/" + label + ".plist\n\tstate = running\n}\n"), nil
	case answerUnknown:
	}
	return []byte("launchctl could not answer\n"), errors.New("exit status 1")
}
