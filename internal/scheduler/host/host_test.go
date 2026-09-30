package host

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/launchd"
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

// Any other system has no scheduler yet, and never runs a program or changes a
// job: every job's state is unknown, and loading or stopping one is refused
// with a message that says why.
func TestNewRefusesSystemsWithoutAScheduler(t *testing.T) {
	t.Parallel()
	site, ref := scheduler.Site{UserHome: "/home/me"}, scheduler.Ref("com.agent-archive.collector")
	var unset platform.OS
	for _, system := range []platform.OS{platform.Linux, platform.Unknown, unset} {
		s := New(system, failRunner(t))
		if got := s.Inspect(context.Background(), site, ref); got.State != scheduler.Unknown || got.Problem == nil || got.Problem.Kind != scheduler.ProblemCannotTell {
			t.Errorf("%q: Inspect = %+v, want unknown with a problem", system, got)
		}
		for name, err := range map[string]error{"Load": s.Load(context.Background(), site, ref), "Unload": s.Unload(context.Background(), site, ref)} {
			if err == nil || !strings.Contains(err.Error(), "no scheduler for this system yet") {
				t.Errorf("%q: %s = %v, want a refusal", system, name, err)
			}
		}
	}
}

// The default is this system's own scheduler: launchd on macOS, none yet
// elsewhere. Making it runs nothing.
func TestDefaultIsThisSystemsScheduler(t *testing.T) {
	t.Parallel()
	_, isLaunchd := Default().(launchd.Scheduler)
	if want := platform.Current() == platform.Darwin; isLaunchd != want {
		t.Errorf("Default() is launchd: %v, want %v on %s", isLaunchd, want, platform.Current())
	}
}
