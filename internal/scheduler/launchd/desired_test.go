package launchd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/scheduler"
)

// Plan renders the collector's plist exactly as LaunchAgent does, as one
// private file in the user's LaunchAgents folder named by the label, and does
// nothing else: the path is a file artifact, and nothing was asked of launchctl
// or written.
func TestPlanIsTheCollectorsPlist(t *testing.T) {
	t.Parallel()
	site := scheduler.Site{UserHome: filepath.Join(t.TempDir(), "home")}
	inst := scheduler.Installation{DataHome: "/data/other", Default: false}
	spec := scheduler.JobSpec{Executable: "/opt/bin/agent-archive", Args: []string{"_collect"}, DataHome: "/data/other", Env: map[string]string{"PATH": "/usr/bin"}, Interval: time.Minute, RunAtLoad: true}
	s := Scheduler{Run: func(context.Context, string, ...string) ([]byte, error) {
		t.Error("Plan ran launchctl")
		return nil, nil
	}}
	plan, err := s.Plan(site, inst, spec)
	must(t, err)
	label := CollectorLabel("/data/other", "")
	want, err := LaunchAgent("/opt/bin/agent-archive", "/data/other", label, map[string]string{"PATH": "/usr/bin"})
	must(t, err)
	if plan.Ref != scheduler.Ref(label) || len(plan.Artifacts) != 1 {
		t.Fatalf("plan %+v, want the job %s in one artifact", plan, label)
	}
	artifact := plan.Artifacts[0]
	if path, ok := artifact.Path(); !ok || path != PlistPath(site, plan.Ref) || string(artifact.After) != string(want) || artifact.Mode != 0o600 {
		t.Errorf("artifact %s (mode %v) differs from LaunchAgent's plist at %s", artifact.ID, artifact.Mode, PlistPath(site, plan.Ref))
	}
	if _, err := os.Stat(site.UserHome); !os.IsNotExist(err) {
		t.Errorf("Plan wrote under the user home (%v)", err)
	}
	inst.Default = true
	if plan, err = s.Plan(site, inst, spec); err != nil || plan.Ref != LaunchLabel {
		t.Errorf("the default installation's job is %q (%v), want %s", plan.Ref, err, LaunchLabel)
	}
}

// Only the collector's job is defined under launchd: anything else in the
// spec is refused, not rendered as the collector.
func TestPlanRefusesAJobThatIsNotTheCollector(t *testing.T) {
	t.Parallel()
	good := scheduler.JobSpec{Executable: "/bin/agent-archive", Args: []string{"_collect"}, DataHome: "/data", Interval: time.Minute, RunAtLoad: true}
	site, inst := scheduler.Site{UserHome: "/home"}, scheduler.Installation{DataHome: "/data", Default: true}
	if _, err := (Scheduler{}).Plan(site, inst, good); err != nil {
		t.Fatalf("the collector's spec: %v", err)
	}
	for name, mutate := range map[string]func(*scheduler.JobSpec){
		"other arguments":   func(s *scheduler.JobSpec) { s.Args = []string{"_hook"} },
		"no arguments":      func(s *scheduler.JobSpec) { s.Args = nil },
		"another interval":  func(s *scheduler.JobSpec) { s.Interval = time.Hour },
		"no run at load":    func(s *scheduler.JobSpec) { s.RunAtLoad = false },
		"relative program":  func(s *scheduler.JobSpec) { s.Executable = "agent-archive" },
		"relative data":     func(s *scheduler.JobSpec) { s.DataHome = "data" },
		"the data variable": func(s *scheduler.JobSpec) { s.Env = map[string]string{"AGENT_ARCHIVE_HOME": "/elsewhere"} },
	} {
		spec := good
		mutate(&spec)
		if _, err := (Scheduler{}).Plan(site, inst, spec); err == nil {
			t.Errorf("%s: Plan rendered %+v", name, spec)
		}
	}
}

// A refusal to stop a job says the words it always has, and names the job and
// the plist, as typed errors carrying the facts behind them.
func TestUnloadRefusalsAreTypedAndSayWhatTheyAlwaysDid(t *testing.T) {
	t.Parallel()
	site, ref := scheduler.Site{UserHome: "/home/me"}, scheduler.Ref("com.agent-archive.collector")
	plist := PlistPath(site, ref)
	for _, tc := range []struct {
		name   string
		output string
		err    error
		want   string
		typed  any
	}{
		{"another", "\tpath = /home/other/Library/LaunchAgents/com.agent-archive.collector.plist\n", nil,
			"launchd's com.agent-archive.collector job was not loaded from " + plist + "; it belongs to another installation and was left running", new(*scheduler.NotOwnedError)},
		{"unknown", "boom", errors.New("exit status 1"),
			"cannot confirm which plist launchd's com.agent-archive.collector job was loaded from; it was left as it is", new(*scheduler.IndeterminateError)},
	} {
		r := &recorder{answer: func(context.Context, ...string) ([]byte, error) { return []byte(tc.output), tc.err }}
		err := Scheduler{Run: r.run}.Unload(context.Background(), site, ref)
		if err == nil || err.Error() != tc.want || !errors.As(err, tc.typed) {
			t.Errorf("%s: Unload = %v, want %q as %T", tc.name, err, tc.want, tc.typed)
		}
		if len(r.calls) != 1 {
			t.Errorf("%s: launchctl calls %q, want only the print", tc.name, r.calls)
		}
	}
}

// Inspect says what launchctl and the plist say, and the facts behind a job
// no command can act on: the plist it expected, the one launchd ran it from,
// and the fix.
func TestInspectCarriesTheFactsOfAProblem(t *testing.T) {
	t.Parallel()
	site, ref := scheduler.Site{UserHome: t.TempDir()}, scheduler.Ref("com.agent-archive.collector")
	plist := PlistPath(site, ref)
	other := "/home/other/Library/LaunchAgents/com.agent-archive.collector.plist"
	inspect := func(output string, err error) scheduler.Status {
		r := &recorder{answer: func(context.Context, ...string) ([]byte, error) { return []byte(output), err }}
		return Scheduler{Run: r.run}.Inspect(context.Background(), site, ref)
	}
	got := inspect("\tpath = "+other+"\n\tstate = running\n", nil)
	want := &scheduler.Problem{Kind: scheduler.ProblemNotOwned, Ref: ref, LoadedFrom: other, Expected: plist,
		Fix: "Uninstall that installation first, or set AGENT_ARCHIVE_HOME to a directory of this installation's own"}
	if got.State != scheduler.AnotherInstallation || got.Problem == nil || *got.Problem != *want {
		t.Errorf("another installation's job: %q with %+v, want %+v", got.State, got.Problem, want)
	}
	got = inspect("no", errors.New("exit status 1"))
	want = &scheduler.Problem{Kind: scheduler.ProblemCannotTell, Ref: ref, Expected: plist, Fix: "Check that launchctl print gui/$(id -u) works in Terminal"}
	if got.State != scheduler.Unknown || got.Problem == nil || *got.Problem != *want {
		t.Errorf("unknown job: %q with %+v, want %+v", got.State, got.Problem, want)
	}
	if got = inspect("path = "+plist+"\nstate = running\n", nil); got.Problem != nil || got.State != scheduler.Running {
		t.Errorf("running job: %q with %+v", got.State, got.Problem)
	}
}

// A plist is read in parts: the program and the environment are parsed apart,
// so one that cannot be read does not hide the other, and the error is the
// first one, which refresh reports.
func TestDefinitionReadsWhatItCan(t *testing.T) {
	t.Parallel()
	site, ref := scheduler.Site{UserHome: t.TempDir()}, scheduler.Ref("com.agent-archive.collector")
	plist := PlistPath(site, ref)
	write := func(text string) scheduler.Status {
		must(t, os.MkdirAll(filepath.Dir(plist), 0o700))
		must(t, os.WriteFile(plist, []byte(text), 0o600))
		return Scheduler{}.Definition(site, ref)
	}
	if got := (Scheduler{}).Definition(site, ref); got.Defined || got.DefinitionErr != nil || got.Program != "" || got.Env != nil || len(got.Paths) != 1 || got.Paths[0] != plist {
		t.Errorf("no plist: %+v", got)
	}
	whole, err := LaunchAgent("/bin/agent-archive", "/data", string(ref), map[string]string{"PATH": "/usr/bin"})
	must(t, err)
	got := write(string(whole))
	if !got.Defined || got.DefinitionErr != nil || got.Program != "/bin/agent-archive" || got.DataHome != "/data" || len(got.Env) != 1 || got.Env["PATH"] != "/usr/bin" {
		t.Errorf("a whole plist: %+v", got)
	}
	// No program: the environment is still read.
	got = write(`<plist><dict><key>EnvironmentVariables</key><dict><key>PATH</key><string>/bin</string></dict></dict></plist>`)
	if got.Program != "" || got.DefinitionErr == nil || got.Env["PATH"] != "/bin" {
		t.Errorf("a plist with no program: %+v (%v)", got, got.DefinitionErr)
	}
	// A program, then a plist that is cut off: the program is read, and the
	// error is the rest's.
	got = write(`<plist><dict><key>ProgramArguments</key><array><string>/bin/a</string></array><key>EnvironmentVariables</key><dict><key>PATH</key><string>/bin</string></key>`)
	if got.Program != "/bin/a" || got.DefinitionErr == nil || got.Env != nil {
		t.Errorf("a plist cut off after its program: %+v (%v)", got, got.DefinitionErr)
	}
	// A plist that cannot be opened is defined, and unreadable.
	must(t, os.Remove(plist))
	must(t, os.Mkdir(plist, 0o700))
	if got = (Scheduler{}).Definition(site, ref); !got.Defined || got.DefinitionErr == nil {
		t.Errorf("a plist that is a directory: %+v", got)
	}
}

// The scheduler's nouns and identity are launchd's.
func TestSchedulerNamesItselfLaunchd(t *testing.T) {
	t.Parallel()
	s := Scheduler{}
	if s.Name() != "launchd" || s.Words() != (scheduler.Words{Manager: "launchd", Job: "LaunchAgent", Definition: "plist", Tool: "launchctl", Name: "label"}) || s.DefaultPATH() != "/usr/bin:/bin:/usr/sbin:/sbin" {
		t.Errorf("Name %q, Words %+v, DefaultPATH %q", s.Name(), s.Words(), s.DefaultPATH())
	}
}

// A plist is a job at a site only when it is <user home>/Library/LaunchAgents/<label>.plist,
// as every plist a release has journaled is, however $HOME is spelled; any
// other path names no job, and is refused rather than taken for another plist.
func TestLocateNamesTheJobAndSiteOfAPlist(t *testing.T) {
	t.Parallel()
	for _, userHome := range []string{"/Users/me", "/Users/me/", "/Users/./me/../me", "/", "me", "./me/", "."} {
		for _, label := range []string{LaunchLabel, LaunchLabel + ".0123456789ab", LegacyLaunchLabel} {
			plist := filepath.Join(userHome, "Library", "LaunchAgents", label+".plist")
			site, ref, err := Scheduler{}.Locate(plist)
			if err != nil || PlistPath(site, ref) != plist || string(ref) != label || site.UserHome != filepath.Clean(userHome) {
				t.Errorf("Locate(%s) = %+v, %q, %v; want the job %s at %s", plist, site, ref, err, label, filepath.Clean(userHome))
			}
		}
	}
	for _, plist := range []string{
		"/synthetic/job",
		"/Users/me/Library/LaunchDaemons/com.agent-archive.collector.plist",
		"/Users/me/Library/LaunchAgents/com.agent-archive.collector.PLIST",
		"/Users/me/Library/LaunchAgents/../LaunchAgents/com.agent-archive.collector.plist",
	} {
		if _, _, err := (Scheduler{}).Locate(plist); err == nil || !strings.Contains(err.Error(), plist) {
			t.Errorf("Locate(%s) = %v; want a refusal that names it", plist, err)
		}
	}
}
