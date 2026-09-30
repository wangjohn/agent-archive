package host

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/launchd"
	"github.com/wangjohn/agent-archive/internal/scheduler/systemd"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

// failRunner stops the test if a scheduler runs anything.
func failRunner(t *testing.T) scheduler.Runner {
	t.Helper()
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		t.Errorf("the scheduler ran %s %q", name, args)
		return nil, nil
	}
}

// macOS is launchd, over the Runner it is given, and choosing it runs nothing
// (the hook runtime has a two-second budget).
func TestNewChoosesLaunchdOnDarwin(t *testing.T) {
	// Should New ever run the real launchctl instead, it finds none.
	t.Setenv("PATH", t.TempDir())
	var calls []string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return []byte("Could not find service"), errors.New("exit status 113")
	}
	got := New(platform.Darwin, run)
	s, ok := got.(launchd.Scheduler)
	if !ok {
		t.Fatalf("New(Darwin) = %T, want launchd.Scheduler", got)
	}
	if len(calls) != 0 {
		t.Fatalf("New ran %q", calls)
	}
	if s.ChangeTimeout != 0 {
		t.Errorf("launchd scheduler %+v, want the default change timeout", s)
	}
	// It runs launchctl through the Runner it was given, never the real one.
	site, ref := scheduler.Site{UserHome: "/Users/me"}, scheduler.Ref("com.agent-archive.collector")
	if state := got.Inspect(context.Background(), site, ref).State; state != scheduler.Missing || len(calls) != 1 || !strings.HasPrefix(calls[0], "launchctl print ") {
		t.Errorf("Inspect = %q after %q, want missing through the given Runner", state, calls)
	}
}

// Linux is systemd, over the Runner it is given, and choosing it runs nothing
// (the hook runtime has a two-second budget). Its journal says "systemd".
func TestNewChoosesSystemdOnLinux(t *testing.T) {
	t.Parallel()
	got := New(platform.Linux, failRunner(t))
	s, ok := got.(systemd.Scheduler)
	if !ok {
		t.Fatalf("New(Linux) = %T, want systemd.Scheduler", got)
	}
	if got.Name() != "systemd" {
		t.Errorf("Name() = %q, want systemd: a Linux setup journal must never say launchd", got.Name())
	}
	var calls []string
	s.Run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return []byte("Failed to connect to bus"), errors.New("exit status 1")
	}
	site, ref := scheduler.Site{UserHome: "/home/me"}, scheduler.Ref("agent-archive-collector")
	if status := s.Inspect(context.Background(), site, ref); status.State != scheduler.Unknown || len(calls) == 0 || !strings.HasPrefix(calls[0], "systemctl ") {
		t.Errorf("Inspect = %+v after %q, want unknown through systemctl", status, calls)
	}
}

// A system with no adapter has a scheduler that says so in its own name, never
// launchd's (a journal it wrote would be recovered as a macOS one), and never
// runs a program or changes a job: every job's state is unknown with a
// problem, and loading or stopping one is refused.
func TestNewRefusesSystemsWithoutAScheduler(t *testing.T) {
	t.Parallel()
	site, ref := scheduler.Site{UserHome: "/tmp/me"}, scheduler.Ref("agent-archive-collector")
	var unset platform.OS
	for _, system := range []platform.OS{platform.Unknown, unset} {
		s := New(system, failRunner(t))
		if s.Name() != "none" {
			t.Errorf("%q: Name() = %q, want none", system, s.Name())
		}
		got := s.Inspect(context.Background(), site, ref)
		if got.State != scheduler.Unknown || got.Problem == nil || got.Problem.Kind != scheduler.ProblemCannotTell || got.Problem.Reason == "" || got.Problem.Fix == "" {
			t.Errorf("%q: Inspect = %+v, want unknown with a problem that says what to do", system, got)
		}
		if _, err := s.Plan(site, scheduler.Installation{DataHome: "/tmp/data", Default: true}, scheduler.JobSpec{}); err == nil {
			t.Errorf("%q: Plan defined a job", system)
		}
		if jobs, err := s.Installed(context.Background(), site, scheduler.Installation{}); err != nil || len(jobs) != 1 {
			t.Errorf("%q: Installed = %v, %v, want the installation's own job", system, jobs, err)
		}
		if s.Definition(site, ref).Defined {
			t.Errorf("%q: Definition found a definition", system)
		}
		if _, _, err := s.Locate("/tmp/me/x.plist"); err == nil {
			t.Errorf("%q: Locate placed a definition", system)
		}
		for name, err := range map[string]error{"Load": s.Load(context.Background(), site, ref), "Unload": s.Unload(context.Background(), site, ref)} {
			if err == nil || !strings.Contains(err.Error(), "no background scheduler for this system") {
				t.Errorf("%q: %s = %v, want a refusal", system, name, err)
			}
		}
	}
}

// The lookup of no name is this system's own scheduler: launchd on macOS,
// systemd on Linux, none elsewhere. Making it runs nothing.
func TestLookupOfNoNameIsThisSystemsScheduler(t *testing.T) {
	t.Parallel()
	want := map[platform.OS]string{platform.Darwin: "launchd", platform.Linux: "systemd", platform.Unknown: "none"}[platform.Current()]
	got, err := Lookup("")
	if err != nil || got.Name() != want {
		t.Errorf("Lookup(\"\") = %v, %v, want %q on %s", got, err, want, platform.Current())
	}
	if _, err := Lookup("cron"); err == nil {
		t.Error("Lookup(cron) found a scheduler")
	}
}

// A backend recorded by name gets that backend's adapter on the system that
// has its manager, the system's own for no name, and an error otherwise: a
// launchd job on Linux (where there is no launchctl) or a scheduler this build
// has never heard of is not one to ask about.
func TestNamedResolvesARecordedBackend(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		system platform.OS
		name   string
		want   string // the scheduler's Name, or "" for an error
	}{
		{platform.Darwin, "", "launchd"},
		{platform.Darwin, "launchd", "launchd"},
		{platform.Linux, "", "systemd"},
		{platform.Linux, "systemd", "systemd"},
		{platform.Unknown, "", "none"},
		{platform.Unknown, "none", "none"},
		{platform.Linux, "none", ""},
		{platform.Darwin, "systemd", ""},
		{platform.Linux, "launchd", ""},
		{platform.Unknown, "launchd", ""},
		{platform.Linux, "cron", ""},
		{platform.Darwin, "none", ""},
	} {
		got, err := Named(tc.system, tc.name, failRunner(t))
		switch {
		case tc.want == "" && err == nil:
			t.Errorf("Named(%q, %q) = %s, want an error", tc.system, tc.name, got.Name())
		case tc.want != "" && (err != nil || got.Name() != tc.want):
			t.Errorf("Named(%q, %q) = %v, %v, want %s", tc.system, tc.name, got, err, tc.want)
		}
	}
	if _, err := Named(platform.Linux, "launchd", failRunner(t)); err == nil || !strings.Contains(err.Error(), "launchd") || !strings.Contains(err.Error(), "linux") {
		t.Errorf("the error for launchd on Linux = %v, want it to name both", err)
	}
}

// Setup records the backend it used in config.json, except launchd, which an
// absent field already means on macOS: a macOS config.json never changes.
func TestRecordedLeavesOutLaunchd(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{"launchd": "", "systemd": "systemd", "none": "none", "": ""} {
		if got := Recorded(name); got != want {
			t.Errorf("Recorded(%q) = %q, want %q", name, got, want)
		}
	}
}

// An unavailable scheduler carries the name it is given, which a journal or a
// configuration may have recorded, and the reason as the fix.
func TestUnavailableNamesItself(t *testing.T) {
	t.Parallel()
	s := Unavailable("cron", "the cron scheduler is not available on this system (linux)", "Run agent-archive setup here")
	if s.Name() != "cron" {
		t.Errorf("Name() = %q, want cron", s.Name())
	}
	status := s.Inspect(context.Background(), scheduler.Site{}, "agent-archive-collector")
	if status.Problem == nil || !strings.Contains(status.Problem.Reason, "cron scheduler is not available") || status.Problem.Fix != "Run agent-archive setup here" {
		t.Errorf("Inspect = %+v, want the reason and the fix", status)
	}
}

// systemctl and loginctl are run with the variables that find the user's
// manager passed on and colors off; any other program gets the environment
// unchanged.
func TestManagerEnvironment(t *testing.T) {
	t.Parallel()
	environ := []string{"HOME=/home/me", "XDG_RUNTIME_DIR=/run/user/1000", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus", "SYSTEMD_COLORS=true", "SYSTEMD_COLORS_EXTRA=1"}
	got := managerEnvironment(environ)
	want := []string{"HOME=/home/me", "XDG_RUNTIME_DIR=/run/user/1000", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus", "SYSTEMD_COLORS_EXTRA=1", "SYSTEMD_COLORS=0"}
	if !slices.Equal(got, want) {
		t.Errorf("managerEnvironment = %q, want %q", got, want)
	}
	if !slices.Equal(environ[:2], []string{"HOME=/home/me", "XDG_RUNTIME_DIR=/run/user/1000"}) || environ[3] != "SYSTEMD_COLORS=true" {
		t.Errorf("managerEnvironment changed its argument: %q", environ)
	}
}
