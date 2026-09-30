package systemd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

// The unit files are in <home>/.config/systemd/user, the directory the user
// manager searches unless its own environment (not the shell's) sets
// XDG_CONFIG_HOME, and Locate finds a job's site and ref from either file's
// path, for any home, the root included.
func TestUnitDirAndLocate(t *testing.T) {
	t.Parallel()
	s := Scheduler{}
	for home, dir := range map[string]string{
		"/home/u":      "/home/u/.config/systemd/user",
		"/tmp/sandbox": "/tmp/sandbox/.config/systemd/user",
		"/":            "/.config/systemd/user",
	} {
		site := scheduler.Site{UserHome: home}
		if got := s.UnitDir(site); got != dir {
			t.Errorf("UnitDir(%s) = %s, want %s", home, got, dir)
		}
		for _, inst := range []scheduler.Installation{{DataHome: "/data", Default: true}, {DataHome: "/other"}} {
			_, spec := collector(inst.DataHome)
			plan, err := s.Plan(site, inst, spec)
			must(t, err)
			for i, ext := range []string{".service", ".timer"} {
				path, _ := plan.Artifacts[i].Path()
				if path != filepath.Join(dir, string(plan.Ref)+ext) {
					t.Errorf("artifact %d is %s, want the %s in %s", i, path, ext, dir)
				}
				if gotSite, ref, err := s.Locate(path); err != nil || gotSite != site || ref != plan.Ref {
					t.Errorf("Locate(%s) = %+v, %q, %v; want %+v, %q", path, gotSite, ref, err, site, plan.Ref)
				}
			}
		}
	}
	// A path no plan writes is refused.
	for _, path := range []string{
		"/home/u/.config/systemd/user/other.timer",
		"/home/u/.config/systemd/user/agent-archive-collector.socket",
		"/home/u/.config/systemd/user/agent-archive-collector-0123.service",
		"/home/u/.config/systemd/agent-archive-collector.timer",
		"/cfg/systemd/user/agent-archive-collector.timer",
		"/home/u/config/systemd/user/agent-archive-collector.timer",
		"/home/u/.config/systemd/user/../user/agent-archive-collector.timer",
		"home/u/.config/systemd/user/agent-archive-collector.timer",
		".config/systemd/user/agent-archive-collector.service",
		"/home/u/.config/systemd/user/agent-archive-collector.service/x.timer",
	} {
		if site, ref, err := s.Locate(path); err == nil {
			t.Errorf("Locate(%s) = %+v, %q; want a refusal", path, site, ref)
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

// Inspect maps what `systemctl show` says of the timer and the service to the
// job's state, for the output of each systemd version:
//
//	timer active                      loaded (whatever the service's last result)
//	service activating or active      running
//	timer inactive, or not found      missing
//	masked, or a unit that fails      unknown, with a Problem
//	loaded from another unit file     another_installation
//
// and notes a drop-in that overrides the unit as Degraded, but not one every
// unit of its type has (a distribution's service.d).
func TestInspectStateMap(t *testing.T) {
	t.Parallel()
	for _, shown := range []string{"239", "245", "252", "255"} {
		for _, tc := range []struct {
			fixture  string
			want     scheduler.JobState
			problem  scheduler.ProblemKind
			mention  string
			degraded string
		}{
			{"loaded", scheduler.Loaded, "", "", ""},
			{"running", scheduler.Running, "", "", ""},
			{"missing", scheduler.Missing, "", "", ""},
			{"inactive", scheduler.Missing, "", "", ""},
			{"failed-service", scheduler.Loaded, "", "", ""},
			{"dropin", scheduler.Loaded, "", "", "override.conf"},
			{"another", scheduler.AnotherInstallation, scheduler.ProblemNotOwned, "/home/someone/.config/systemd/user/agent-archive-collector.timer", ""},
			{"masked", scheduler.Unknown, scheduler.ProblemCannotTell, "systemctl --user unmask", ""},
			{"typewide-dropin", scheduler.Loaded, "", "", ""},
		} {
			if _, err := os.Stat(filepath.Join(fixtures, shown, "show-"+tc.fixture+".txt")); tc.fixture == "typewide-dropin" && err != nil {
				continue // captured on 255 alone
			}
			t.Run(shown+"/"+tc.fixture, func(t *testing.T) {
				t.Parallel()
				f := newFakeSystemctl(t, "252")
				f.shown, f.override = shown, tc.fixture
				s, site := Scheduler{Run: f.run}, scheduler.Site{UserHome: t.TempDir()}
				ref := write(t, s, site)
				f.UseSite(site)
				got := s.Inspect(context.Background(), site, ref)
				if got.State != tc.want {
					t.Fatalf("state %q, want %q", got.State, tc.want)
				}
				if (got.Problem == nil) != (tc.problem == "") || got.Problem != nil && (got.Problem.Kind != tc.problem || got.Problem.Ref != ref || !strings.Contains(got.Problem.LoadedFrom+got.Problem.Fix, tc.mention)) {
					t.Errorf("problem %+v, want a %q one naming %q", got.Problem, tc.problem, tc.mention)
				}
				if (tc.degraded == "") != (len(got.Degraded) == 0) || len(got.Degraded) > 0 && !strings.Contains(got.Degraded[0], tc.degraded) {
					t.Errorf("degraded %q, want it to mention %q", got.Degraded, tc.degraded)
				}
			})
		}
	}
}

// A systemd older than 240 cannot append to the collector's logs, unless it
// is RHEL 8's 239 from the release that backported append: on: Inspect says
// so, with the fix, and nothing is stopped for a job it cannot describe.
func TestOldSystemdIsRefused(t *testing.T) {
	t.Parallel()
	for version, refused := range map[string]bool{
		"systemd 238\n+PAM":                 true,
		"systemd 239\n+PAM":                 true,
		"systemd 239 (239)\n":               true,
		"systemd 239 (239-31.el8)\n":        true,
		"systemd 239 (239-45.fc29)\n":       true,
		"systemd 237 (237-3ubuntu10.57)\n":  true,
		"systemd 239 (239-32.el8)\n":        false,
		"systemd 239 (239-82.el8_10.19)\n":  false,
		"systemd 240 (240)\n":               false,
		"systemd 255 (255.4-1ubuntu8.17)\n": false,
		"systemd 257.7 (257.7-1)\n":         false, // not a number: asked as if new
	} {
		f := newFakeSystemctl(t, "252")
		f.versionText = version
		s, site := Scheduler{Run: f.run}, scheduler.Site{UserHome: t.TempDir()}
		ref := write(t, s, site)
		f.Put(site, ref, scheduler.Loaded)
		got := s.Inspect(context.Background(), site, ref)
		if !refused {
			if got.State != scheduler.Loaded {
				t.Errorf("Inspect on %q: %q, %+v, want it asked", version, got.State, got.Problem)
			}
			continue
		}
		if got.State != scheduler.Unknown || got.Problem == nil || got.Problem.Kind != scheduler.ProblemCannotTell || !strings.Contains(got.Problem.Fix, "240") || !strings.Contains(got.Problem.Fix, "RHEL 8") {
			t.Errorf("Inspect on %q: %q, %+v", version, got.State, got.Problem)
		}
		var indeterminate *scheduler.IndeterminateError
		if err := s.Unload(context.Background(), site, ref); !errors.As(err, &indeterminate) {
			t.Errorf("Unload on %q: %v", version, err)
		}
		if calls := f.Calls(); slices.ContainsFunc(calls, func(c string) bool { return strings.Contains(c, "show") || strings.Contains(c, "disable") }) {
			t.Errorf("systemctl was asked about the job on %q: %q", version, calls)
		}
	}
}

// With no user bus (an SSH session, a container), systemctl fails, and the
// job is unknown, with a Problem whose fix says how to get a user manager;
// with no systemctl at all it says to install systemd.
func TestNoUserBusIsUnknownWithAFix(t *testing.T) {
	t.Parallel()
	site, ref := scheduler.Site{UserHome: t.TempDir()}, scheduler.Ref("agent-archive-collector")
	for name, tc := range map[string]struct {
		run  scheduler.Runner
		fix  []string
		none string
	}{
		"no bus": {func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if args[0] == "--version" {
				return []byte("systemd 252 (252.22)\n"), nil
			}
			return []byte("Failed to connect to user scope bus via local transport: $DBUS_SESSION_BUS_ADDRESS and $XDG_RUNTIME_DIR not defined\n"), errors.New("exit status 1")
		}, []string{"enable-linger", "login session"}, ""},
		"not booted with systemd": {func(context.Context, string, ...string) ([]byte, error) {
			return []byte("System has not been booted with systemd as init system (PID 1). Can't operate.\n"), errors.New("exit status 1")
		}, []string{"enable-linger"}, ""},
		"no systemctl": {func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New(`exec: "systemctl": executable file not found in $PATH`)
		}, []string{"Install systemd"}, ""},
		"some other failure": {func(context.Context, string, ...string) ([]byte, error) {
			return []byte("boom"), errors.New("exit status 1")
		}, []string{"systemctl --user status"}, ""},
	} {
		got := Scheduler{Run: tc.run}.Inspect(context.Background(), site, ref)
		if got.State != scheduler.Unknown || got.Problem == nil || got.Problem.Kind != scheduler.ProblemCannotTell || got.Problem.Ref != ref {
			t.Fatalf("%s: %q, %+v", name, got.State, got.Problem)
		}
		for _, want := range tc.fix {
			if !strings.Contains(got.Problem.Fix, want) {
				t.Errorf("%s: fix %q does not mention %q", name, got.Problem.Fix, want)
			}
		}
	}
}

// A job that runs works only while the user is logged in when lingering is
// off, and Inspect says so; a job that is not running, or a loginctl that
// cannot say, says nothing.
func TestLingeringOffIsDegraded(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		fixture string
		linger  string
		want    bool
	}{{"loaded", "no", true}, {"running", "no", true}, {"loaded", "yes", false}, {"missing", "no", false}, {"loaded", "", false}} {
		f := newFakeSystemctl(t, "252")
		f.override, f.linger = tc.fixture, tc.linger
		run := f.run
		if tc.linger == "" { // no loginctl
			run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
				if name == "loginctl" {
					return nil, errors.New("not found")
				}
				return f.run(ctx, name, args...)
			}
		}
		site := scheduler.Site{UserHome: t.TempDir()}
		f.UseSite(site)
		got := Scheduler{Run: run}.Inspect(context.Background(), site, "agent-archive-collector")
		if has := len(got.Degraded) == 1 && strings.Contains(got.Degraded[0], "enable-linger"); has != tc.want || len(got.Degraded) > 1 {
			t.Errorf("%s job, lingering %q: degraded %q, want a lingering note %v", tc.fixture, tc.linger, got.Degraded, tc.want)
		}
	}
}

// Load reloads the manager, which has not seen the unit files, then enables
// the timer and starts it; a failure carries systemctl's own words.
func TestLoadReloadsThenEnablesTheTimer(t *testing.T) {
	t.Parallel()
	f := newFakeSystemctl(t, "252")
	s, site := Scheduler{Run: f.run}, scheduler.Site{UserHome: t.TempDir()}
	ref := write(t, s, site)
	f.UseSite(site)
	must(t, s.Load(context.Background(), site, ref))
	if want := []string{"systemctl --user daemon-reload", "systemctl --user enable --now " + string(ref) + ".timer"}; !slices.Equal(f.Calls(), want) {
		t.Errorf("calls %q, want %q", f.Calls(), want)
	}
	must(t, os.Remove(s.timerPath(site, ref)))
	err := s.Load(context.Background(), site, ref)
	if err == nil || !strings.Contains(err.Error(), "systemctl enable") || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("Load of a missing unit file: %v", err)
	}
	if err := s.Load(context.Background(), site, "not-a-job"); err == nil {
		t.Error("Load accepted a ref no plan makes")
	}
}

// Unload asks first, and stops the job only when systemd loaded it from these
// unit files: it disables and stops the timer, stops the service and clears
// its failed state, and reloads the manager. It leaves another installation's job, and one it cannot
// describe, alone, and does nothing for a job that is not loaded.
func TestUnloadStopsOnlyItsOwnJob(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		state   scheduler.JobState
		want    []string
		wantErr string
	}{
		{scheduler.Loaded, []string{"disable --now", "stop", "reset-failed", "daemon-reload"}, ""},
		{scheduler.Running, []string{"disable --now", "stop", "reset-failed", "daemon-reload"}, ""},
		{scheduler.Missing, nil, ""},
		{scheduler.AnotherInstallation, nil, "belongs to another installation"},
		{scheduler.Unknown, nil, "cannot confirm"},
	} {
		f := newFakeSystemctl(t, "252")
		s, site := Scheduler{Run: f.run}, scheduler.Site{UserHome: t.TempDir()}
		ref := write(t, s, site)
		f.Put(site, ref, tc.state)
		err := s.Unload(context.Background(), site, ref)
		if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
			t.Fatalf("%s: err %v, want %q", tc.state, err, tc.wantErr)
		}
		var changes []string
		for _, call := range f.Calls() {
			if verb, ok := strings.CutPrefix(call, "systemctl --user "); ok && !strings.HasPrefix(verb, "show") {
				verb, _, _ = strings.Cut(verb, " "+string(ref))
				changes = append(changes, verb)
			}
		}
		if !slices.Equal(changes, tc.want) {
			t.Errorf("%s: changes %q, want %q", tc.state, changes, tc.want)
		}
	}
}

// seenCall is what a call's context looked like: the time it had left and its
// error.
type seenCall struct {
	left time.Duration
	err  error
	ok   bool
}

// answering is a Runner that answers as a healthy systemd whose job is loaded
// from timer and service, records each call's context by its verb, and hangs
// on the verb hang.
type answering struct {
	ref     string
	timer   string
	service string
	hang    string
	seen    map[string]seenCall
}

func (a *answering) run(ctx context.Context, _ string, args ...string) ([]byte, error) {
	verb := args[0]
	if verb == "--user" {
		verb = args[1]
	}
	deadline, ok := ctx.Deadline()
	if a.seen == nil {
		a.seen = map[string]seenCall{}
	}
	a.seen[verb] = seenCall{time.Until(deadline), ctx.Err(), ok}
	switch verb {
	case a.hang:
		<-ctx.Done()
		return nil, ctx.Err()
	case "--version":
		return []byte("systemd 252 (252.22)\n"), nil
	case "show":
		return []byte("Id=" + a.ref + ".timer\nLoadState=loaded\nActiveState=active\nFragmentPath=" + a.timer + "\n\nId=" + a.ref + ".service\nLoadState=loaded\nActiveState=inactive\nFragmentPath=" + a.service + "\n"), nil
	}
	return nil, nil
}

// A load and an unload run on a bounded context of their own that the caller's
// cancellation and deadline never reach: an interrupted setup still finishes
// the change it started. A question is the caller's to cancel.
func TestChangesAreBoundedAndIgnoreTheCallersCancellation(t *testing.T) {
	t.Parallel()
	site, ref := scheduler.Site{UserHome: "/home/u"}, scheduler.Ref("agent-archive-collector")
	answer := func() *answering {
		return &answering{ref: string(ref), timer: Scheduler{}.timerPath(site, ref), service: Scheduler{}.servicePath(site, ref)}
	}
	a := answer()
	s := Scheduler{Run: a.run}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	cancel()
	must(t, s.Load(ctx, site, ref))
	must(t, s.Unload(ctx, site, ref))
	for verb, bound := range map[string]time.Duration{"enable": ChangeTimeout, "disable": ChangeTimeout, "stop": ChangeTimeout, "daemon-reload": ChangeTimeout, "show": stateTimeout} {
		if got := a.seen[verb]; !got.ok || got.err != nil || got.left < bound-5*time.Second || got.left > bound {
			t.Errorf("systemctl %s: deadline %v, context error %v, %v to its deadline; want an uncancelled context bounded by %v", verb, got.ok, got.err, got.left, bound)
		}
	}
	s.Inspect(ctx, site, ref)
	if got := a.seen["--version"]; got.err == nil {
		t.Errorf("the question in Inspect ran uncancelled with %v to its deadline", got.left)
	}

	// A systemctl that hangs ends on its own.
	for verb, run := range map[string]func(Scheduler) error{
		"enable":  func(s Scheduler) error { return s.Load(context.Background(), site, ref) },
		"disable": func(s Scheduler) error { return s.Unload(context.Background(), site, ref) },
	} {
		a := answer()
		a.hang = verb
		done := make(chan error, 1)
		go func() { done <- run(Scheduler{Run: a.run, ChangeTimeout: 20 * time.Millisecond}) }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "systemctl "+verb) {
				t.Errorf("%s: %v", verb, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s hung: nothing bounds systemctl", verb)
		}
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
	var _ scheduler.Scheduler = s
}

// Each unit counts on its own: a service another installation's unit files
// define is another installation's job even under our timer, and a service
// that is stopping is still running, so a stop reaches it, even when its timer
// is gone.
func TestInspectJudgesEachUnit(t *testing.T) {
	t.Parallel()
	site, ref := scheduler.Site{UserHome: "/home/u"}, scheduler.Ref("agent-archive-collector")
	timer, service := Scheduler{}.timerPath(site, ref), Scheduler{}.servicePath(site, ref)
	id := "Id=" + string(ref)
	for name, tc := range map[string]struct {
		show string
		want scheduler.JobState
	}{
		"another service":            {id + ".timer\nLoadState=loaded\nActiveState=active\nFragmentPath=" + timer + "\n\n" + id + ".service\nLoadState=loaded\nActiveState=inactive\nFragmentPath=/home/x/.config/systemd/user/" + string(ref) + ".service\n", scheduler.AnotherInstallation},
		"stopping service, no timer": {id + ".timer\nLoadState=not-found\nActiveState=inactive\n\n" + id + ".service\nLoadState=loaded\nActiveState=deactivating\nFragmentPath=" + service + "\n", scheduler.Running},
		"a loaded unit with no file": {id + ".timer\nLoadState=loaded\nActiveState=active\n\n" + id + ".service\nLoadState=not-found\nActiveState=inactive\n", scheduler.Unknown},
		// Captured on systemd 255: the unit files deleted and the manager
		// reloaded while the job ran. Unload cannot stop a unit it cannot
		// tell is its own, so it must not say there is nothing to stop.
		"running with its files gone":         {id + ".timer\nLoadState=not-found\nActiveState=active\nSubState=running\nFragmentPath=\n\n" + id + ".service\nLoadState=not-found\nActiveState=activating\nSubState=start\nFragmentPath=\n", scheduler.Unknown},
		"a timer with its file gone":          {id + ".timer\nLoadState=not-found\nActiveState=active\n\n" + id + ".service\nLoadState=not-found\nActiveState=inactive\n", scheduler.Unknown},
		"a failed service whose file is gone": {id + ".timer\nLoadState=not-found\nActiveState=inactive\n\n" + id + ".service\nLoadState=loaded\nActiveState=failed\nFragmentPath=" + service + "\n", scheduler.Missing},
	} {
		run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if args[0] == "--version" {
				return []byte("systemd 252 (252.22)\n"), nil
			}
			return []byte(tc.show), nil
		}
		s := Scheduler{Run: run}
		got := s.Inspect(context.Background(), site, ref)
		if got.State != tc.want {
			t.Errorf("%s: state %q (%+v), want %q", name, got.State, got.Problem, tc.want)
		}
		var indeterminate *scheduler.IndeterminateError
		if err := s.Unload(context.Background(), site, ref); tc.want == scheduler.Unknown && !errors.As(err, &indeterminate) {
			t.Errorf("%s: Unload %v, want an *IndeterminateError", name, err)
		}
	}
}

// Clearing the failed state of a service Unload stopped is tidying: when
// systemctl refuses it (the manager already unloaded the unit), the unload
// still succeeds and still reloads the manager.
func TestUnloadSucceedsWhenResetFailedIsRefused(t *testing.T) {
	t.Parallel()
	site, ref := scheduler.Site{UserHome: "/home/u"}, scheduler.Ref("agent-archive-collector")
	a := &answering{ref: string(ref), timer: Scheduler{}.timerPath(site, ref), service: Scheduler{}.servicePath(site, ref)}
	var calls []string
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if slices.Contains(args, "reset-failed") {
			return []byte("Failed to reset failed state of unit agent-archive-collector.service: Unit agent-archive-collector.service not loaded.\n"), errors.New("exit status 1")
		}
		return a.run(ctx, name, args...)
	}
	must(t, Scheduler{Run: run}.Unload(context.Background(), site, ref))
	if last := calls[len(calls)-1]; last != "--user daemon-reload" {
		t.Errorf("calls %q end without a reload", calls)
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
			// Private, as launchd's plists are: the environment may hold a
			// proxy address with a password in it.
			if mode := plan.Artifacts[i].Mode; mode != 0o600 {
				t.Errorf("%s: the %s is mode %o, want 0600", name, ext, mode)
			}
			golden.Check(t, filepath.Join("testdata", "conformance", name+ext), plan.Artifacts[i].After)
		}
	}
}
