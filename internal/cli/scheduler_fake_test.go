package cli

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/wangjohn/agent-archive/internal/scheduler"
)

// fakeScheduler is the scheduler every test but the ones of the real launchd
// code stands in with (Env.Scheduler). It names jobs by ref alone, so a test
// says which job it means (jobRef of a plist it holds, or a label) and never
// where its definition lives.
//
// It behaves as launchd does for the states the commands tell apart: a job
// not otherwise set is in def ("missing" unless the test says), load makes a
// job "loaded" and unload makes it "missing". Every call is recorded, in
// order, as "state REF", "load REF" or "unload REF". The tests of the real
// launchd code (the argv characterization) do not use it: they stub launchctl
// (stubLaunchctl) and drive launchd.Scheduler through a nil Env.Scheduler.
type fakeScheduler struct {
	t *testing.T
	// forbidChanges makes a load or unload fail the test (and the call), for
	// a test that must not start or stop a job (testEnv's).
	forbidChanges bool
	// stateFn, when set, answers jobState in place of the job's set state.
	stateFn func(ref scheduler.Ref) string
	// beforeLoad and beforeUnload run when the call is made; an error they
	// return is the call's, and the job's state stays as it was.
	beforeLoad   func(ref scheduler.Ref) error
	beforeUnload func(ref scheduler.Ref) error

	mu     sync.Mutex
	def    string
	states map[scheduler.Ref]string
	calls  []string
}

// newFakeScheduler is a scheduler whose jobs are all in state (or "missing"
// when it is empty) until a test sets them, or loads or unloads them.
func newFakeScheduler(t *testing.T, state string) *fakeScheduler {
	t.Helper()
	if state == "" {
		state = "missing"
	}
	return &fakeScheduler{t: t, def: state, states: map[scheduler.Ref]string{}}
}

// fakeSched is the fakeScheduler of env, a test environment that has one.
func fakeSched(env Env) *fakeScheduler { return env.Scheduler.(*fakeScheduler) }

// noLaunchd is the scheduler testEnv starts with: every job is missing, and
// loading or stopping one is a test failure, as reaching the real one would be.
func noLaunchd(t *testing.T) *fakeScheduler {
	t.Helper()
	f := newFakeScheduler(t, "missing")
	f.forbidChanges = true
	return f
}

// set puts ref's job in state.
func (f *fakeScheduler) set(ref scheduler.Ref, state string) *fakeScheduler {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[ref] = state
	return f
}

// state is the state the fake holds for ref, whatever stateFn says.
func (f *fakeScheduler) state(ref scheduler.Ref) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stateLocked(ref)
}

func (f *fakeScheduler) stateLocked(ref scheduler.Ref) string {
	if state, ok := f.states[ref]; ok {
		return state
	}
	return f.def
}

func (f *fakeScheduler) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeScheduler) JobState(_ context.Context, _ scheduler.Site, ref scheduler.Ref) scheduler.JobState {
	f.record("state " + string(ref))
	if f.stateFn != nil {
		return scheduler.JobState(f.stateFn(ref))
	}
	return scheduler.JobState(f.state(ref))
}

func (f *fakeScheduler) Load(_ context.Context, _ scheduler.Site, ref scheduler.Ref) error {
	f.record("load " + string(ref))
	if f.forbidChanges {
		f.t.Errorf("unexpected load of the %s job: set Env.Scheduler", ref)
		return errors.New("no launchd in this test")
	}
	if f.beforeLoad != nil {
		if err := f.beforeLoad(ref); err != nil {
			return err
		}
	}
	f.set(ref, "loaded")
	return nil
}

func (f *fakeScheduler) Unload(_ context.Context, _ scheduler.Site, ref scheduler.Ref) error {
	f.record("unload " + string(ref))
	if f.forbidChanges {
		f.t.Errorf("unexpected unload of the %s job: set Env.Scheduler", ref)
		return errors.New("no launchd in this test")
	}
	if f.beforeUnload != nil {
		if err := f.beforeUnload(ref); err != nil {
			return err
		}
	}
	f.set(ref, "missing")
	return nil
}

// all is every call so far, in order.
func (f *fakeScheduler) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// forget clears the record of calls.
func (f *fakeScheduler) forget() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// changing is the calls that start or stop a job (not the questions).
func (f *fakeScheduler) changing() []string {
	var out []string
	for _, call := range f.all() {
		if !strings.HasPrefix(call, "state ") {
			out = append(out, call)
		}
	}
	return out
}

// loaded and unloaded are the refs of the load and unload calls made, in order.
func (f *fakeScheduler) loaded() []scheduler.Ref { return f.refsOf("load ") }

func (f *fakeScheduler) unloaded() []scheduler.Ref { return f.refsOf("unload ") }

func (f *fakeScheduler) refsOf(prefix string) []scheduler.Ref {
	var refs []scheduler.Ref
	for _, call := range f.all() {
		if ref, ok := strings.CutPrefix(call, prefix); ok {
			refs = append(refs, scheduler.Ref(ref))
		}
	}
	return refs
}
