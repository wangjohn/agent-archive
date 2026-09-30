package systemd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/scheduler"
)

// A job systemd cannot be asked about says what is wrong in a clause of its
// own (the commands put it in their sentence, in place of a launchd-shaped
// "did not say"), apart from the next step.
func TestUnknownJobsSayWhatIsWrong(t *testing.T) {
	t.Parallel()
	site, ref := scheduler.Site{UserHome: t.TempDir()}, scheduler.Ref("agent-archive-collector")
	const manual = "systemctl --user stop agent-archive-collector.timer agent-archive-collector.service"
	version := func(text string, then scheduler.Runner) scheduler.Runner {
		return func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if len(args) == 1 && args[0] == "--version" {
				return []byte(text), nil
			}
			return then(ctx, name, args...)
		}
	}
	fail := func(text string) scheduler.Runner {
		return func(context.Context, string, ...string) ([]byte, error) {
			return []byte(text), errors.New("exit status 1")
		}
	}
	// What the manager says about the units themselves, from the captured
	// fixtures.
	f := newFakeSystemctl(t, "252")
	f.override = "masked"
	got := Scheduler{Run: f.run}.Inspect(context.Background(), site, ref)
	if got.State != scheduler.Unknown || got.Problem == nil || !strings.Contains(got.Problem.Reason, "is masked") || got.Problem.Manual != manual {
		t.Errorf("masked: %q, %+v, want a reason that says it is masked and the manual stop", got.State, got.Problem)
	}
	for name, tc := range map[string]struct {
		run    scheduler.Runner
		reason string
	}{
		"no user bus":  {version("systemd 252 (252.22)\n", fail("Failed to connect to user scope bus via local transport\n")), "no user bus"},
		"not systemd":  {fail("System has not been booted with systemd as init system (PID 1). Can't operate.\n"), "not booted with systemd"},
		"no systemctl": {fail(`exec: "systemctl": executable file not found in $PATH`), "systemctl was not found"},
		"too old":      {version("systemd 237 (237-3ubuntu10)\n", fail("unused")), "systemd 237, older than 240"},
		"some failure": {fail("boom"), "systemctl did not answer"},
		"no units":     {version("systemd 252 (252.22)\n", func(context.Context, string, ...string) ([]byte, error) { return []byte("\n"), nil }), "did not describe the job's units"},
	} {
		got := Scheduler{Run: tc.run}.Inspect(context.Background(), site, ref)
		if got.State != scheduler.Unknown || got.Problem == nil || !strings.Contains(got.Problem.Reason, tc.reason) || got.Problem.Fix == "" {
			t.Errorf("%s: %q, %+v, want a reason that says %q and a fix", name, got.State, got.Problem, tc.reason)
		}
		// Whatever the reason, the command that stops the job by hand is what
		// Unload does, for a session that can reach the manager.
		if got.Problem != nil && got.Problem.Manual != manual {
			t.Errorf("%s: manual stop %q, want %q", name, got.Problem.Manual, manual)
		}
	}
}
