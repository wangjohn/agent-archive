package systemd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// collector is the default installation's job as setup would define it.
func collector(dataHome string) (scheduler.Installation, scheduler.JobSpec) {
	return scheduler.Installation{DataHome: dataHome, Default: true}, scheduler.JobSpec{
		Executable: "/usr/local/bin/agent-archive", Args: []string{"_collect"}, DataHome: dataHome,
		Env: map[string]string{"PATH": "/usr/bin:/bin"}, Interval: time.Minute, RunAtLoad: true,
	}
}

// write plans the collector at site and writes its unit files, as setup does.
func write(t *testing.T, s Scheduler, site scheduler.Site) scheduler.Ref {
	t.Helper()
	inst, spec := collector(filepath.Join(site.UserHome, "data"))
	plan, err := s.Plan(site, inst, spec)
	must(t, err)
	for _, artifact := range plan.Artifacts {
		path, _ := artifact.Path()
		must(t, os.MkdirAll(filepath.Dir(path), 0o700))
		must(t, os.WriteFile(path, artifact.After, artifact.Mode))
	}
	return plan.Ref
}

// A job's ref is its unit name, and another installation's ends in the digest
// launchd's label ends in for the same directory (launchd's CollectorLabel
// gives com.agent-archive.collector.6a14b5407408 for it).
func TestRefIsTheUnitName(t *testing.T) {
	t.Parallel()
	s := Scheduler{}
	if got := s.Ref(scheduler.Installation{DataHome: "/x", Default: true}); got != "agent-archive-collector" {
		t.Errorf("the default installation's ref is %q", got)
	}
	if got := s.Ref(scheduler.Installation{DataHome: "/Users/test/other data/"}); got != "agent-archive-collector-6a14b5407408" {
		t.Errorf("another installation's ref is %q", got)
	}
	for ref, want := range map[scheduler.Ref]bool{
		"agent-archive-collector": true, "agent-archive-collector-0123456789ab": true,
		"agent-archive-collector-0123456789AB": false, "agent-archive-collector-0123": false, "agent-archive-collectorx": false,
		"-agent-archive-collector": false, "agent-archive-collector/../x": false, "": false,
	} {
		if validRef(ref) != want {
			t.Errorf("validRef(%q) = %v", ref, !want)
		}
	}
}

// The unit files are where systemd looks for the user's own: under
// $XDG_CONFIG_HOME when the process has it as an absolute path and the site is
// the process's own home, else under <home>/.config. A site that is not the
// process's home (a sandbox, a test) never reaches the real configuration
// directory, and Locate finds a job's site from either kind of path.
func TestUnitDirHonorsXDGConfigHome(t *testing.T) {
	t.Parallel()
	site := scheduler.Site{UserHome: "/home/u"}
	for _, tc := range []struct {
		name string
		env  map[string]string
		site scheduler.Site
		dir  string
	}{
		{"no XDG_CONFIG_HOME", map[string]string{"HOME": "/home/u"}, site, "/home/u/.config/systemd/user"},
		{"an absolute XDG_CONFIG_HOME", map[string]string{"HOME": "/home/u", "XDG_CONFIG_HOME": "/cfg"}, site, "/cfg/systemd/user"},
		{"a relative XDG_CONFIG_HOME", map[string]string{"HOME": "/home/u", "XDG_CONFIG_HOME": "cfg"}, site, "/home/u/.config/systemd/user"},
		{"another home than the process's", map[string]string{"HOME": "/home/u", "XDG_CONFIG_HOME": "/cfg"}, scheduler.Site{UserHome: "/tmp/sandbox"}, "/tmp/sandbox/.config/systemd/user"},
		{"no HOME", map[string]string{"XDG_CONFIG_HOME": "/cfg"}, site, "/home/u/.config/systemd/user"},
	} {
		s := Scheduler{Getenv: func(name string) string { return tc.env[name] }}
		if got := s.UnitDir(tc.site); got != tc.dir {
			t.Errorf("%s: unit directory %s, want %s", tc.name, got, tc.dir)
		}
		inst, spec := collector("/data")
		plan, err := s.Plan(tc.site, inst, spec)
		must(t, err)
		for _, artifact := range plan.Artifacts {
			path, _ := artifact.Path()
			if gotSite, ref, err := s.Locate(path); err != nil || gotSite != tc.site || ref != plan.Ref || filepath.Dir(path) != tc.dir {
				t.Errorf("%s: Locate(%s) = %+v, %q, %v; want %+v, %q", tc.name, path, gotSite, ref, err, tc.site, plan.Ref)
			}
		}
	}
	// A path in a directory the unit directory is not now is refused: a
	// journal recorded while XDG_CONFIG_HOME was another is left as it is.
	s := Scheduler{Getenv: func(name string) string { return map[string]string{"HOME": "/home/u", "XDG_CONFIG_HOME": "/cfg"}[name] }}
	for _, path := range []string{"/home/u/.config/systemd/user/agent-archive-collector.service", "/cfg/systemd/user/other.timer", "/cfg/systemd/user/agent-archive-collector.socket", "/cfg/systemd/agent-archive-collector.timer", "/cfg/systemd/user/../user/agent-archive-collector.timer"} {
		if _, _, err := s.Locate(path); err == nil {
			t.Errorf("Locate(%s) accepted a path no plan writes now", path)
		}
	}
}

// Definition reads the unit files alone: it asks systemd nothing, so a drop-in
// that overrides the unit never reaches what refresh writes back, and a unit
// with a service but no timer, or a timer but no service, is defined and says
// what is missing.
func TestDefinitionReadsTheUnitFilesAlone(t *testing.T) {
	t.Parallel()
	s := Scheduler{Run: func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("Definition asked the manager")
	}}
	site := scheduler.Site{UserHome: t.TempDir()}
	ref := write(t, s, site)
	got := s.Definition(site, ref)
	if !got.Defined || got.DefinitionErr != nil || got.Program != "/usr/local/bin/agent-archive" || got.DataHome != filepath.Join(site.UserHome, "data") || got.Env["PATH"] != "/usr/bin:/bin" || len(got.Env) != 1 {
		t.Errorf("Definition = %+v (%v)", got, got.DefinitionErr)
	}
	must(t, os.Remove(s.timerPath(site, ref)))
	if got := s.Definition(site, ref); !got.Defined || got.DefinitionErr != nil || got.Program == "" {
		t.Errorf("a service with no timer reads %+v (%v)", got, got.DefinitionErr)
	}
	write(t, s, site)
	must(t, os.Remove(s.servicePath(site, ref)))
	if got := s.Definition(site, ref); !got.Defined || got.DefinitionErr == nil || got.Program != "" || len(got.Paths) != 2 {
		t.Errorf("a timer with no service reads %+v (%v)", got, got.DefinitionErr)
	}
	if got := s.Definition(site, "agent-archive-collector/../x"); got.Defined || got.DefinitionErr == nil {
		t.Errorf("a ref no plan makes reads %+v", got)
	}
}

// The names systemd calls the adapter's things are the ones messages use.
func TestWordsAndNames(t *testing.T) {
	t.Parallel()
	s := Scheduler{}
	if got, want := s.Words(), (scheduler.Words{Manager: "systemd", Job: "user timer", Definition: "unit file", Tool: "systemctl"}); got != want || s.Name() != "systemd" {
		t.Errorf("words %+v, name %q", got, s.Name())
	}
	if !strings.Contains(s.DefaultPATH(), "/usr/bin") || strings.HasPrefix(s.DefaultPATH(), ":") {
		t.Errorf("DefaultPATH %q", s.DefaultPATH())
	}
}

// A unit file is what a user's disk holds for as long as the job is installed,
// so its bytes are recorded: the default installation's (a plain job, a
// variable) and one whose paths and values each need quoting (a space, a
// specifier's percent sign, a dollar sign, quotes and a backslash). A change to
// them is reviewed in the golden diff.
func TestUnitFilesAreTheRecordedOnes(t *testing.T) {
	t.Parallel()
	site, home := scheduler.Site{UserHome: "/Users/test"}, "/Users/test"
	collect := func(executable, dataHome string, env map[string]string) scheduler.JobSpec {
		return scheduler.JobSpec{Executable: executable, Args: []string{"_collect"}, DataHome: dataHome, Env: env, Interval: time.Minute, RunAtLoad: true}
	}
	plain, quoted := home+"/.local/share/agent-archive", home+`/100% $HOME's data`
	for name, tc := range map[string]struct {
		inst scheduler.Installation
		spec scheduler.JobSpec
	}{
		"default": {scheduler.Installation{DataHome: plain, Default: true}, collect(home+"/bin/agent-archive", plain, map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin"})},
		"needs-quoting": {scheduler.Installation{DataHome: quoted}, collect(home+"/My Apps/agent-archive", quoted, map[string]string{
			"AWS_PROFILE": `it's "work" \ 50%`, "PATH": "/opt/$tools/bin:/usr/bin"})},
	} {
		plan, err := Scheduler{}.Plan(site, tc.inst, tc.spec)
		must(t, err)
		if len(plan.Artifacts) != 2 {
			t.Fatalf("%s: %d artifacts, want the service and the timer", name, len(plan.Artifacts))
		}
		for i, ext := range []string{".service", ".timer"} {
			if path, _ := plan.Artifacts[i].Path(); !strings.HasSuffix(path, string(plan.Ref)+ext) {
				t.Errorf("%s: artifact %d is %s, want the %s", name, i, path, ext)
			}
			golden.Check(t, filepath.Join("testdata", "conformance", name+ext), plan.Artifacts[i].After)
		}
	}
}
