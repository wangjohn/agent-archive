package launchd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/scheduler"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

// recorder is the Runner of these tests: launchctl is never run, every call
// is recorded as its arguments, and answer says what each call prints. It
// also records the time left on each call's context.
type recorder struct {
	calls  [][]string
	seen   map[string]seenCall
	answer func(ctx context.Context, args ...string) ([]byte, error)
}

type seenCall struct {
	err  error
	left time.Duration
	ok   bool
}

func (r *recorder) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name != "launchctl" {
		return nil, fmt.Errorf("ran %q, not launchctl", name)
	}
	r.calls = append(r.calls, args)
	deadline, ok := ctx.Deadline()
	if r.seen == nil {
		r.seen = map[string]seenCall{}
	}
	r.seen[args[0]] = seenCall{err: ctx.Err(), left: time.Until(deadline), ok: ok}
	return r.answer(ctx, args...)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// A job's definition is <user home>/Library/LaunchAgents/<label>.plist, and
// its ref is that label: PlistPath and Label are each other's inverse.
func TestPlistPathAndLabelAreInverse(t *testing.T) {
	t.Parallel()
	site := scheduler.Site{UserHome: "/Users/me"}
	plist := PlistPath(site, "com.agent-archive.collector.0123456789ab")
	if want := "/Users/me/Library/LaunchAgents/com.agent-archive.collector.0123456789ab.plist"; plist != want {
		t.Errorf("PlistPath = %s, want %s", plist, want)
	}
	if got := Label(plist); got != "com.agent-archive.collector.0123456789ab" {
		t.Errorf("Label = %s", got)
	}
	if got, want := ServiceTarget(plist), fmt.Sprintf("gui/%d/com.agent-archive.collector.0123456789ab", os.Getuid()); got != want {
		t.Errorf("ServiceTarget = %s, want %s", got, want)
	}
}

// Load bootstraps the job's plist into this user's GUI session and reports
// launchctl's own output when that fails. launchctl is not run.
func TestLoadBootstrapsThePlist(t *testing.T) {
	t.Parallel()
	site, ref := scheduler.Site{UserHome: t.TempDir()}, scheduler.Ref("com.agent-archive.collector.abc")
	plist := PlistPath(site, ref)
	r := &recorder{answer: func(context.Context, ...string) ([]byte, error) { return nil, nil }}
	must(t, Scheduler{Run: r.run}.Load(context.Background(), site, ref))
	want := []string{"bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), plist}
	if len(r.calls) != 1 || !slices.Equal(r.calls[0], want) {
		t.Fatalf("launchctl calls %q, want %q", r.calls, want)
	}

	r = &recorder{answer: func(context.Context, ...string) ([]byte, error) {
		return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
	}}
	err := Scheduler{Run: r.run}.Load(context.Background(), site, ref)
	if err == nil || !strings.Contains(err.Error(), "launchctl bootstrap") || !strings.Contains(err.Error(), "Input/output error") {
		t.Fatalf("err %v, want launchctl's output", err)
	}
}

// Unload boots out a job only once launchctl print shows it was loaded from
// this very plist, by its service target; a job loaded from any other plist,
// or one launchctl cannot describe, is left alone and reported.
func TestUnloadBootsOutOnlyItsOwnJob(t *testing.T) {
	t.Parallel()
	site, ref := scheduler.Site{UserHome: t.TempDir()}, scheduler.Ref("com.agent-archive.collector.abc")
	plist := PlistPath(site, ref)
	other := filepath.Join(t.TempDir(), "com.agent-archive.collector.abc.plist")
	target := fmt.Sprintf("gui/%d/com.agent-archive.collector.abc", os.Getuid())
	for _, tc := range []struct {
		name      string
		print     string
		printErr  error
		bootErr   error
		bootedOut bool
		wantErr   string
	}{
		{name: "running from this plist", print: "path = " + plist + "\nstate = running\n", bootedOut: true},
		{name: "loaded from this plist", print: "path = " + plist + "\nstate = waiting\n", bootedOut: true},
		{name: "not loaded", print: "Could not find service", printErr: errors.New("exit status 113")},
		{name: "loaded from another plist", print: "path = " + other + "\nstate = running\n", wantErr: "belongs to another installation"},
		{name: "print names no plist", print: "state = running\n", wantErr: "cannot confirm"},
		{name: "print fails", print: "some failure", printErr: errors.New("exit status 1"), wantErr: "cannot confirm"},
		{name: "bootout fails", print: "path = " + plist + "\nstate = running\n", bootErr: errors.New("exit status 5"), bootedOut: true, wantErr: "launchctl bootout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &recorder{answer: func(_ context.Context, args ...string) ([]byte, error) {
				if args[0] == "print" {
					return []byte(tc.print), tc.printErr
				}
				return []byte("bootout output"), tc.bootErr
			}}
			err := Scheduler{Run: r.run}.Unload(context.Background(), site, ref)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err %v, want %q", err, tc.wantErr)
			}
			if !slices.Equal(r.calls[0], []string{"print", target}) {
				t.Fatalf("first call %q", r.calls[0])
			}
			bootedOut := len(r.calls) == 2 && slices.Equal(r.calls[1], []string{"bootout", target})
			if bootedOut != tc.bootedOut || len(r.calls) > 2 {
				t.Fatalf("launchctl calls %q, booted out %v, want %v", r.calls, bootedOut, tc.bootedOut)
			}
		})
	}
}

// A label is not proof of ownership: launchd reports the plist it loaded a
// job from, and a job loaded from any other file is another installation's.
func TestParseJobStateComparesTheLoadedPlist(t *testing.T) {
	t.Parallel()
	ours := filepath.Join(t.TempDir(), "com.agent-archive.collector.plist")
	for _, tc := range []struct {
		output string
		err    error
		want   scheduler.JobState
	}{
		{"gui/501/com.agent-archive.collector = {\n\tpath = " + ours + "\n\tstate = running\n}", nil, scheduler.Running},
		{"\tstate = waiting\n\tpath = " + ours + "\n", nil, scheduler.Loaded},
		{"\tpath = /Users/someone/Library/LaunchAgents/com.agent-archive.collector.plist\n\tstate = running\n", nil, scheduler.AnotherInstallation},
		{"\tstate = running\n", nil, scheduler.Unknown},
		{"Could not find service \"x\" in domain for user gui: 501", fmt.Errorf("exit status 113"), scheduler.Missing},
		{"boom", fmt.Errorf("exit status 1"), scheduler.Unknown},
		// A print killed at its deadline may have written the path already.
		{"\tpath = " + ours + "\n\tstate = running\n", fmt.Errorf("signal: killed"), scheduler.Unknown},
	} {
		if got := ParseJobState(tc.output, tc.err, ours); got != tc.want {
			t.Errorf("%q: %s, want %s", tc.output, got, tc.want)
		}
	}
}

// launchctl bootstrap and bootout end on their own: refresh absorbs Ctrl-C
// and SIGTERM while it runs them, so a launchctl that hangs must not leave a
// process only SIGKILL stops.
func TestChangesAreBounded(t *testing.T) {
	t.Parallel()
	site, ref := scheduler.Site{UserHome: t.TempDir()}, scheduler.Ref("com.example.collector")
	plist := PlistPath(site, ref)
	r := &recorder{answer: func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == "print" {
			return []byte("path = " + plist + "\nstate = running\n"), nil
		}
		<-ctx.Done() // a hung bootstrap or bootout
		return nil, ctx.Err()
	}}
	s := Scheduler{Run: r.run, ChangeTimeout: 20 * time.Millisecond}
	for name, run := range map[string]func(context.Context, scheduler.Site, scheduler.Ref) error{"bootstrap": s.Load, "bootout": s.Unload} {
		done := make(chan error, 1)
		go func() { done <- run(context.Background(), site, ref) }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "launchctl "+name) {
				t.Errorf("%s: %v", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s hung: nothing bounds launchctl", name)
		}
	}
}

// Load and Unload run on a bounded context of their own that the caller's
// cancellation and deadline never reach: a setup that was interrupted (or
// whose own context ran out) still finishes the launchctl change it started,
// and the setup journal handles what comes after.
func TestChangesIgnoreTheCallersCancellation(t *testing.T) {
	t.Parallel()
	site, ref := scheduler.Site{UserHome: "/Users/me"}, scheduler.Ref("com.agent-archive.collector")
	plist := PlistPath(site, ref)
	r := &recorder{answer: func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "print" {
			return []byte("path = " + plist + "\nstate = running\n"), nil
		}
		return nil, nil
	}}
	s := Scheduler{Run: r.run}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	cancel()

	must(t, s.Load(ctx, site, ref))
	must(t, s.Unload(ctx, site, ref))

	for _, tc := range []struct {
		verb string
		min  time.Duration
		max  time.Duration
	}{
		{"print", time.Second, 2 * time.Second},
		{"bootstrap", 25 * time.Second, ChangeTimeout},
		{"bootout", 25 * time.Second, ChangeTimeout},
	} {
		got := r.seen[tc.verb]
		if !got.ok || got.err != nil || got.left < tc.min || got.left > tc.max {
			t.Errorf("launchctl %s: deadline %v, context error %v, %v to its deadline; want an uncancelled context with between %v and %v", tc.verb, got.ok, got.err, got.left, tc.min, tc.max)
		}
	}

	// Asking changes nothing, so a question alone is the caller's to cancel.
	delete(r.seen, "print")
	s.JobState(ctx, site, ref)
	if got := r.seen["print"]; got.err == nil {
		t.Errorf("launchctl print for JobState ran uncancelled with %v to its deadline", got.left)
	}
}

// The bound on a change is the one refresh's hung-launchctl behavior was
// characterized with.
func TestChangeTimeoutIsThirtySeconds(t *testing.T) {
	t.Parallel()
	if ChangeTimeout != 30*time.Second {
		t.Errorf("ChangeTimeout = %v", ChangeTimeout)
	}
}
