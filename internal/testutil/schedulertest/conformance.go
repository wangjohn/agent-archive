package schedulertest

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
)

// Manager is the manager behind the scheduler a conformance run drives: the
// suite puts a job in a state through it, and reads back what the scheduler
// did. An adapter's Manager is its fake Runner, answering as the real tool
// does (launchd: a launchctl that prints, bootstraps and boots out); the Model
// is its own. Like the real tool, a fake Runner fails when its context is
// done, and a stop stops whatever job of that name is loaded, whosever
// definition it came from, so the suite sees a change that should not have
// happened.
type Manager interface {
	// Put makes the manager report the job ref at site as state. A job in
	// AnotherInstallation is one loaded from a definition that is not site's,
	// and Unknown one the manager cannot describe.
	Put(site scheduler.Site, ref scheduler.Ref, state scheduler.JobState)
	// Held is the state the manager holds for the job, which a load or an
	// unload changes: Missing for a job it was never given.
	Held(site scheduler.Site, ref scheduler.Ref) scheduler.JobState
	// Calls is everything the manager was asked or told, in order, in any
	// words: the suite compares its length, to see that a call did nothing.
	Calls() []string
}

// Backend is one scheduler for the suite to run over.
type Backend struct {
	// New returns a scheduler and the manager behind it, both with no job.
	New func(t *testing.T) (scheduler.Scheduler, Manager)
	// Golden checks got, the bytes of one definition, against the recorded
	// output of this backend, for the definition name.
	Golden func(t *testing.T, name string, got []byte)
	// Earlier writes, under site, the definition of a job an earlier release
	// of the tool left for inst's data directory under another name than the
	// installation's own, and returns its ref. A backend with no such history
	// leaves it nil.
	Earlier func(t *testing.T, site scheduler.Site, inst scheduler.Installation) scheduler.Ref
}

// specimen is a job the suite defines: an installation and what it runs.
type specimen struct {
	name string
	inst scheduler.Installation
	spec scheduler.JobSpec
}

// specimens are the jobs the suite defines, under home (a folder that need
// not exist: Plan reads nothing).
func specimens(home string) []specimen {
	collect := func(dataHome string, env map[string]string) scheduler.JobSpec {
		return scheduler.JobSpec{Executable: filepath.Join(home, "bin", "agent-archive"), Args: []string{"_collect"}, DataHome: dataHome, Env: env, Interval: time.Minute, RunAtLoad: true}
	}
	return []specimen{
		{"default", scheduler.Installation{DataHome: filepath.Join(home, ".local", "share", "agent-archive"), Default: true},
			collect(filepath.Join(home, ".local", "share", "agent-archive"), map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin"})},
		{"other-directory", scheduler.Installation{DataHome: filepath.Join(home, "other data")},
			collect(filepath.Join(home, "other data"), map[string]string{"AWS_CONFIG_FILE": filepath.Join(home, ".aws", "config"), "AWS_PROFILE": "work", "PATH": "/opt/tools/bin:/usr/bin"})},
		{"no-environment", scheduler.Installation{DataHome: filepath.Join(home, "plain"), Default: true},
			collect(filepath.Join(home, "plain"), nil)},
		{"needs-escaping", scheduler.Installation{DataHome: filepath.Join(home, `a & <b> "c"`)},
			collect(filepath.Join(home, `a & <b> "c"`), map[string]string{"HTTPS_PROXY": "http://proxy.example:3128/?a&b<2>", "PATH": "/usr/bin"})},
		// What a unit file or a shell line would read as something else: a
		// space in the program's path, a specifier (%), a variable ($), quotes
		// and a backslash.
		{"needs-quoting", scheduler.Installation{DataHome: filepath.Join(home, `100% $HOME's data`)},
			scheduler.JobSpec{Executable: filepath.Join(home, "My Apps", "agent-archive"), Args: []string{"_collect"}, DataHome: filepath.Join(home, `100% $HOME's data`), Env: map[string]string{"AWS_PROFILE": `it's "work" \ 50%`, "PATH": `/opt/$tools/bin:/usr/bin`}, Interval: time.Minute, RunAtLoad: true}},
	}
}

// RunConformance runs the conformance suite over b: what every adapter of the
// scheduler port must do, so that code written against the port behaves the
// same over any of them. It covers, in order, the state matrix, the refusal to
// stop what another installation owns (with typed errors) and an idempotent
// unload, a load, changes that an interrupt does not cancel, the purity and
// the recorded output of Plan, the round trips from Plan
// through the disk to Inspect and back to Plan (a refresh), that a definition
// holds no credential value, and the problem a job in an unknown state comes
// with.
func RunConformance(t *testing.T, b Backend) {
	t.Helper()
	t.Run("StateMatrix", func(t *testing.T) { stateMatrix(t, b) })
	t.Run("UnloadRefusesWhatItDoesNotOwn", func(t *testing.T) { unloadRefuses(t, b) })
	t.Run("UnloadIsIdempotent", func(t *testing.T) { unloadIdempotent(t, b) })
	t.Run("LoadStartsTheDefinedJob", func(t *testing.T) { loadStarts(t, b) })
	t.Run("ChangesAreNotCancelled", func(t *testing.T) { changesNotCancelled(t, b) })
	t.Run("PlanIsPure", func(t *testing.T) { planPure(t, b) })
	t.Run("PlanOutput", func(t *testing.T) { planOutput(t, b) })
	t.Run("RefIdentifiesTheInstallation", func(t *testing.T) { refIdentifies(t, b) })
	t.Run("PlanInspectRoundTrip", func(t *testing.T) { planInspectRoundTrip(t, b) })
	t.Run("RefreshRoundTrip", func(t *testing.T) { refreshRoundTrip(t, b) })
	t.Run("NoCredentialValuesInADefinition", func(t *testing.T) { noCredentials(t, b) })
	t.Run("UnreadableAndAbsentDefinitions", func(t *testing.T) { unreadableDefinitions(t, b) })
	t.Run("InstalledListsTheInstallationsJobs", func(t *testing.T) { installedJobs(t, b) })
	t.Run("LocateIsTheInverseOfPlan", func(t *testing.T) { locateInverse(t, b) })
}

// site is a user home the suite may write under, and a scheduler over it.
func fresh(t *testing.T, b Backend) (scheduler.Scheduler, Manager, scheduler.Site) {
	t.Helper()
	s, m := b.New(t)
	return s, m, scheduler.Site{UserHome: t.TempDir()}
}

// define plans sp at site and writes what it plans, as setup does.
func define(t *testing.T, s scheduler.Scheduler, site scheduler.Site, sp specimen) scheduler.Plan {
	t.Helper()
	plan, err := s.Plan(site, sp.inst, sp.spec)
	if err != nil {
		t.Fatalf("Plan(%s): %v", sp.name, err)
	}
	for _, artifact := range plan.Artifacts {
		path, ok := artifact.Path()
		if !ok {
			t.Fatalf("Plan(%s): artifact %s is not a file", sp.name, artifact.ID)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, artifact.After, artifact.Mode); err != nil {
			t.Fatal(err)
		}
	}
	return plan
}

var allStates = []scheduler.JobState{scheduler.Loaded, scheduler.Running, scheduler.Missing, scheduler.Unknown, scheduler.AnotherInstallation}

// Whatever the manager says, Inspect says it back, with the facts a command
// needs when it blocks: nothing for a job it can act on, and for the two it
// cannot, a Problem naming the job, the definition it expected (one of the
// job's own), and a next step. Definition says nothing of the manager and
// does not ask it. Both the default installation's job and another's are
// asked about.
func stateMatrix(t *testing.T, b Backend) {
	t.Helper()
	for _, sp := range specimens(t.TempDir())[:2] {
		for _, state := range allStates {
			t.Run(sp.name+"/"+string(state), func(t *testing.T) {
				s, m, site := fresh(t, b)
				plan := define(t, s, site, sp)
				m.Put(site, plan.Ref, state)
				asked := len(m.Calls())
				got := s.Inspect(context.Background(), site, plan.Ref)
				if got.State != state {
					t.Fatalf("Inspect: state %q, want %q", got.State, state)
				}
				if len(m.Calls()) == asked {
					t.Error("Inspect did not ask the manager")
				}
				switch state {
				case scheduler.Unknown:
					if p := got.Problem; p == nil || p.Kind != scheduler.ProblemCannotTell || p.Ref != plan.Ref || !slices.Contains(got.Paths, p.Expected) || p.Fix == "" {
						t.Errorf("a job the manager cannot describe has problem %+v (paths %q)", p, got.Paths)
					}
				case scheduler.AnotherInstallation:
					if p := got.Problem; p == nil || p.Kind != scheduler.ProblemNotOwned || p.Ref != plan.Ref || !slices.Contains(got.Paths, p.Expected) || p.LoadedFrom == "" || p.LoadedFrom == p.Expected || p.Fix == "" {
						t.Errorf("a job another installation owns has problem %+v (paths %q)", p, got.Paths)
					}
				case scheduler.Loaded, scheduler.Running, scheduler.Missing:
					if got.Problem != nil {
						t.Errorf("a %s job has problem %+v", state, got.Problem)
					}
				}
				// The definition is read whatever the state is.
				if !got.Defined || got.Program != sp.spec.Executable {
					t.Errorf("Inspect of a %s job: defined %v, program %q", state, got.Defined, got.Program)
				}
				asked = len(m.Calls())
				definition := s.Definition(site, plan.Ref)
				if definition.State != "" || len(m.Calls()) != asked {
					t.Errorf("Definition asked the manager or answered a state (%q)", definition.State)
				}
			})
		}
	}
}

// Nothing stops a job the manager runs from another installation's
// definition, or one it cannot describe: the refusal is a *NotOwnedError or an
// *IndeterminateError, in the scheduler's own words, with the facts of the
// problem, that says which job it left; and the job is still there.
func unloadRefuses(t *testing.T, b Backend) {
	t.Helper()
	for _, sp := range specimens(t.TempDir())[:2] {
		t.Run(sp.name, func(t *testing.T) {
			s, m, site := fresh(t, b)
			plan := define(t, s, site, sp)
			ctx := context.Background()
			words := s.Words()
			if words.Manager == "" || words.Job == "" || words.Definition == "" || words.Tool == "" {
				t.Errorf("the scheduler's words %+v leave a noun out", words)
			}

			m.Put(site, plan.Ref, scheduler.AnotherInstallation)
			err := s.Unload(ctx, site, plan.Ref)
			var notOwned *scheduler.NotOwnedError
			if !errors.As(err, &notOwned) {
				t.Fatalf("Unload of another installation's job: %v, want a *NotOwnedError", err)
			}
			if p := notOwned.Problem; p.Kind != scheduler.ProblemNotOwned || p.Ref != plan.Ref || p.Expected == "" || p.LoadedFrom == p.Expected || notOwned.Words != words || !strings.Contains(err.Error(), string(plan.Ref)) {
				t.Errorf("the refusal %q (%+v, %+v) does not name the job and the definition it expected, in the scheduler's words", err, p, notOwned.Words)
			}
			if held := m.Held(site, plan.Ref); held != scheduler.AnotherInstallation {
				t.Errorf("another installation's job is %q after the refusal", held)
			}

			m.Put(site, plan.Ref, scheduler.Unknown)
			err = s.Unload(ctx, site, plan.Ref)
			var indeterminate *scheduler.IndeterminateError
			if !errors.As(err, &indeterminate) {
				t.Fatalf("Unload of a job it cannot describe: %v, want an *IndeterminateError", err)
			}
			if p := indeterminate.Problem; p.Kind != scheduler.ProblemCannotTell || p.Ref != plan.Ref || indeterminate.Words != words || !strings.Contains(err.Error(), string(plan.Ref)) {
				t.Errorf("the refusal %q (%+v, %+v) does not name the job in the scheduler's words", err, p, indeterminate.Words)
			}
			if held := m.Held(site, plan.Ref); held != scheduler.Unknown {
				t.Errorf("a job the manager cannot describe is %q after the refusal", held)
			}
		})
	}
}

// Unload stops a job that is its own, and is not an error when there is
// nothing to stop, so stopping twice, or a job never loaded, is the same as
// once.
func unloadIdempotent(t *testing.T, b Backend) {
	t.Helper()
	for _, state := range []scheduler.JobState{scheduler.Loaded, scheduler.Running, scheduler.Missing} {
		t.Run(string(state), func(t *testing.T) {
			s, m, site := fresh(t, b)
			plan := define(t, s, site, specimens(t.TempDir())[0])
			m.Put(site, plan.Ref, state)
			for range 2 {
				if err := s.Unload(context.Background(), site, plan.Ref); err != nil {
					t.Fatalf("Unload of a %s job: %v", state, err)
				}
				if held := m.Held(site, plan.Ref); held != scheduler.Missing {
					t.Fatalf("the job is %q after Unload", held)
				}
			}
		})
	}
}

// Load starts what is on disk, and it is then loaded and this installation's:
// the manager holds it from the definition the scheduler expects.
func loadStarts(t *testing.T, b Backend) {
	t.Helper()
	s, m, site := fresh(t, b)
	plan := define(t, s, site, specimens(t.TempDir())[0])
	if err := s.Load(context.Background(), site, plan.Ref); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if held := m.Held(site, plan.Ref); held != scheduler.Loaded && held != scheduler.Running {
		t.Fatalf("the job is %q after Load", held)
	}
	if got := s.Inspect(context.Background(), site, plan.Ref); got.State != scheduler.Loaded && got.State != scheduler.Running {
		t.Errorf("Inspect after Load: %q", got.State)
	}
}

// A change runs to its end on a context of its own: the caller's
// cancellation (an interrupt of setup) never stops a load or an unload
// halfway, since the setup journal handles what is half applied.
func changesNotCancelled(t *testing.T, b Backend) {
	t.Helper()
	s, m, site := fresh(t, b)
	plan := define(t, s, site, specimens(t.TempDir())[0])
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Load(ctx, site, plan.Ref); err != nil {
		t.Fatalf("Load on a cancelled context: %v", err)
	}
	if held := m.Held(site, plan.Ref); held != scheduler.Loaded && held != scheduler.Running {
		t.Fatalf("the job is %q after Load on a cancelled context", held)
	}
	if err := s.Unload(ctx, site, plan.Ref); err != nil {
		t.Fatalf("Unload on a cancelled context: %v", err)
	}
	if held := m.Held(site, plan.Ref); held != scheduler.Missing {
		t.Errorf("the job is %q after Unload on a cancelled context", held)
	}
}

// Plan is pure: the same installation and spec give the same plan, and it
// reads no file, writes none, and asks the manager nothing, so the shared
// transaction is what reads and applies.
func planPure(t *testing.T, b Backend) {
	t.Helper()
	s, m, _ := fresh(t, b)
	site := scheduler.Site{UserHome: filepath.Join(t.TempDir(), "no", "such", "home")}
	for _, sp := range specimens(t.TempDir()) {
		first, err := s.Plan(site, sp.inst, sp.spec)
		if err != nil {
			t.Fatalf("Plan(%s): %v", sp.name, err)
		}
		second, err := s.Plan(site, sp.inst, sp.spec)
		if err != nil || !reflect.DeepEqual(first, second) {
			t.Errorf("Plan(%s) twice: %v and %+v, want the same plan", sp.name, err, second)
		}
		if first.Ref != s.Ref(sp.inst) {
			t.Errorf("Plan(%s) names the job %q, Ref says %q", sp.name, first.Ref, s.Ref(sp.inst))
		}
		if len(first.Artifacts) == 0 {
			t.Fatalf("Plan(%s) has nothing to write", sp.name)
		}
		for _, artifact := range first.Artifacts {
			path, ok := artifact.Path()
			if !ok || !filepath.IsAbs(path) || !local.PathWithin(path, site.UserHome) {
				t.Errorf("Plan(%s) writes %q, want a file under the user home %s", sp.name, artifact.ID, site.UserHome)
			}
			if artifact.Mode == 0 || artifact.Mode&0o022 != 0 || len(artifact.After) == 0 {
				t.Errorf("Plan(%s) artifact %s: mode %v, %d bytes; a definition is private and not empty", sp.name, artifact.ID, artifact.Mode, len(artifact.After))
			}
		}
	}
	if _, err := os.Stat(site.UserHome); !os.IsNotExist(err) {
		t.Errorf("Plan touched the user home (%v)", err)
	}
	// What is on disk changes nothing: a plan made over another definition of
	// the same job (another program, another environment) is the plan made
	// over none.
	site = scheduler.Site{UserHome: t.TempDir()}
	for _, sp := range specimens(t.TempDir()) {
		want, err := s.Plan(site, sp.inst, sp.spec)
		if err != nil {
			t.Fatalf("Plan(%s): %v", sp.name, err)
		}
		old := sp
		old.spec.Executable = filepath.Join(t.TempDir(), "old", "agent-archive")
		old.spec.Env = map[string]string{"FROM_THE_OLD_DEFINITION": "1"}
		define(t, s, site, old)
		if got, err := s.Plan(site, sp.inst, sp.spec); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("Plan(%s) over another definition: %v, %+v; want the plan made over none", sp.name, err, got)
		}
	}
	if calls := m.Calls(); len(calls) != 0 {
		t.Errorf("Plan asked the manager %q", calls)
	}
}

// Plan writes the recorded bytes of each definition: a change to them is a
// change to what every user's disk holds, so the backend's goldens are the
// review of it.
func planOutput(t *testing.T, b Backend) {
	t.Helper()
	s, _, _ := fresh(t, b)
	site := scheduler.Site{UserHome: "/Users/test"}
	for _, sp := range specimens("/Users/test") {
		plan, err := s.Plan(site, sp.inst, sp.spec)
		if err != nil {
			t.Fatalf("Plan(%s): %v", sp.name, err)
		}
		for i, artifact := range plan.Artifacts {
			name := sp.name
			if i > 0 {
				name += "." + string(rune('0'+i))
			}
			b.Golden(t, name, artifact.After)
		}
	}
}

// A ref is what the installation is called to its scheduler: every default
// installation has the name the default one always had, whatever its
// directory, and every other one a name of its own derived from its
// directory, the same each time it is asked.
func refIdentifies(t *testing.T, b Backend) {
	t.Helper()
	s, _, _ := fresh(t, b)
	def := s.Ref(scheduler.Installation{DataHome: "/Users/test/a", Default: true})
	if def == "" || def != s.Ref(scheduler.Installation{DataHome: "/Users/test/b", Default: true}) {
		t.Errorf("the default installation's ref is %q, and depends on its directory", def)
	}
	one, two := s.Ref(scheduler.Installation{DataHome: "/Users/test/a"}), s.Ref(scheduler.Installation{DataHome: "/Users/test/b"})
	if one == "" || one == two || one == def || two == def {
		t.Errorf("refs of installations of /a, /b and the default: %q, %q, %q; want three different ones", one, two, def)
	}
	if again := s.Ref(scheduler.Installation{DataHome: "/Users/test/a"}); again != one {
		t.Errorf("the ref of one directory changed from %q to %q", one, again)
	}
	for _, sp := range specimens("/Users/test") {
		if got := s.Ref(sp.inst); (got == def) != sp.inst.Default {
			t.Errorf("%s: ref %q, and the default's is %q", sp.name, got, def)
		}
	}
}

// What Plan writes, Inspect reads back: the program, the environment (without
// AGENT_ARCHIVE_HOME, which is the data directory), where it is, and that it
// is there.
func planInspectRoundTrip(t *testing.T, b Backend) {
	t.Helper()
	for _, sp := range specimens(t.TempDir()) {
		t.Run(sp.name, func(t *testing.T) {
			s, m, site := fresh(t, b)
			plan := define(t, s, site, sp)
			m.Put(site, plan.Ref, scheduler.Loaded)
			for name, got := range map[string]scheduler.Status{"Inspect": s.Inspect(context.Background(), site, plan.Ref), "Definition": s.Definition(site, plan.Ref)} {
				want := sp.spec.Env
				if want == nil {
					want = map[string]string{}
				}
				if !got.Defined || got.DefinitionErr != nil || got.Program != sp.spec.Executable || got.DataHome != sp.spec.DataHome || !maps.Equal(got.Env, want) || got.Env == nil {
					t.Errorf("%s reads back %+v (%v), want program %s, data %s, environment %v", name, got, got.DefinitionErr, sp.spec.Executable, sp.spec.DataHome, want)
				}
				if _, has := got.Env["AGENT_ARCHIVE_HOME"]; has {
					t.Errorf("%s: the environment holds AGENT_ARCHIVE_HOME, which is the data directory", name)
				}
				// Paths are what uninstall removes, so every file Plan writes
				// is among them.
				for _, artifact := range plan.Artifacts {
					if path, _ := artifact.Path(); !slices.Contains(got.Paths, path) {
						t.Errorf("%s: paths %q do not hold the definition %s", name, got.Paths, path)
					}
				}
			}
		})
	}
}

// A refresh redefines a job from what its definition says: Status.Env goes
// into JobSpec.Env, and the executable changes. With nothing changed the
// definition is what was there (a refresh normalizes only what it does not
// keep); with a new executable only that changes.
func refreshRoundTrip(t *testing.T, b Backend) {
	t.Helper()
	for _, sp := range specimens(t.TempDir()) {
		t.Run(sp.name, func(t *testing.T) {
			s, _, site := fresh(t, b)
			before := define(t, s, site, sp)
			read := s.Definition(site, before.Ref)
			again := sp
			again.spec.Env = read.Env
			same, err := s.Plan(site, sp.inst, again.spec)
			if err != nil || !reflect.DeepEqual(same, before) {
				t.Fatalf("redefining a job from its own status: %v; %+v, want the plan it was written from", err, same)
			}
			moved := again
			moved.spec.Executable = filepath.Join(t.TempDir(), "bin", "agent-archive")
			define(t, s, site, moved)
			got := s.Definition(site, before.Ref)
			if got.Program != moved.spec.Executable || !maps.Equal(got.Env, read.Env) || got.DataHome != sp.spec.DataHome {
				t.Errorf("after a refresh the definition reads %+v, want program %s and everything else as it was", got, moved.spec.Executable)
			}
		})
	}
}

// A definition is a file in the user's home, so it holds what the JobSpec
// holds and nothing of the process that writes it: a credential in the
// environment of setup never reaches it, and the data directory is not an
// environment variable a caller may set.
func noCredentials(t *testing.T, b Backend) {
	t.Helper()
	secrets := map[string]string{"AWS_SECRET_ACCESS_KEY": "s3cr3t-conformance-secret-key", "AWS_SESSION_TOKEN": "s3cr3t-conformance-session-token", "AWS_ACCESS_KEY_ID": "AKIAconformanceaccesskey"}
	for name, value := range secrets {
		t.Setenv(name, value)
	}
	s, _, site := fresh(t, b)
	for _, sp := range specimens(t.TempDir()) {
		plan := define(t, s, site, sp)
		for _, artifact := range plan.Artifacts {
			for name, value := range secrets {
				if bytes.Contains(artifact.After, []byte(value)) || bytes.Contains(artifact.After, []byte(name)) {
					t.Errorf("the definition of %s holds %s from the process's environment", sp.name, name)
				}
			}
		}
	}
	sp := specimens(t.TempDir())[1]
	sp.spec.Env = map[string]string{"AGENT_ARCHIVE_HOME": "/somewhere/else"}
	if _, err := s.Plan(site, sp.inst, sp.spec); err == nil {
		t.Error("Plan accepted AGENT_ARCHIVE_HOME in the environment: the data directory is the JobSpec's DataHome")
	}
	sp = specimens(t.TempDir())[0]
	sp.spec.Executable = "relative/agent-archive"
	if _, err := s.Plan(site, sp.inst, sp.spec); err == nil {
		t.Error("Plan accepted a relative executable")
	}
}

// A job with no definition is not defined, and no error; one with a
// definition that cannot be read (the read fails) or cannot be understood (it
// is not a definition) is defined, with the error, and its manager state is
// still what the manager says.
func unreadableDefinitions(t *testing.T, b Backend) {
	t.Helper()
	for _, damage := range []struct {
		name string
		do   func(path string) error
	}{
		{"not-a-definition", func(path string) error { return os.WriteFile(path, []byte("this is not a definition"), 0o600) }},
		{"read-fails", func(path string) error {
			// A folder where the file was: its read fails, even as root.
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.Mkdir(path, 0o700)
		}},
	} {
		t.Run(damage.name, func(t *testing.T) {
			s, m, site := fresh(t, b)
			sp := specimens(t.TempDir())[0]
			ref := s.Ref(sp.inst)
			absent := s.Inspect(context.Background(), site, ref)
			if absent.Defined || absent.DefinitionErr != nil || absent.Program != "" || absent.State != scheduler.Missing || len(absent.Paths) == 0 {
				t.Errorf("a job with no definition reads %+v (%v)", absent, absent.DefinitionErr)
			}
			plan := define(t, s, site, sp)
			for _, artifact := range plan.Artifacts {
				path, _ := artifact.Path()
				if err := damage.do(path); err != nil {
					t.Fatal(err)
				}
			}
			m.Put(site, ref, scheduler.Loaded)
			bad := s.Inspect(context.Background(), site, ref)
			if !bad.Defined || bad.DefinitionErr == nil || bad.Program != "" || bad.State != scheduler.Loaded {
				t.Errorf("an unreadable definition reads %+v (%v)", bad, bad.DefinitionErr)
			}
		})
	}
}

// Installed names an installation's own job first, even before it is
// defined, then its aliases; a job of another data directory is never among
// them, and asking changes nothing.
func installedJobs(t *testing.T, b Backend) {
	t.Helper()
	s, m, site := fresh(t, b)
	specs := specimens(t.TempDir())
	own, other := specs[1], specs[3]
	ref := s.Ref(own.inst)
	ctx := context.Background()
	jobs, err := s.Installed(ctx, site, own.inst)
	if err != nil || !slices.Equal(jobs, []scheduler.Job{{Ref: ref}}) {
		t.Fatalf("Installed before anything is defined: %+v, %v; want only the installation's own job %s", jobs, err, ref)
	}
	define(t, s, site, own)
	define(t, s, site, other)
	asked := len(m.Calls())
	jobs, err = s.Installed(ctx, site, own.inst)
	if err != nil || !slices.Equal(jobs, []scheduler.Job{{Ref: ref}}) {
		t.Errorf("Installed with another installation's job on disk: %+v, %v; want only %s", jobs, err, ref)
	}
	if len(m.Calls()) != asked {
		t.Error("Installed asked the manager")
	}
	if b.Earlier == nil {
		return
	}
	earlier := b.Earlier(t, site, own.inst)
	if earlier == "" || earlier == ref {
		t.Fatalf("the earlier job is %q, want a ref of its own", earlier)
	}
	jobs, err = s.Installed(ctx, site, own.inst)
	want := []scheduler.Job{{Ref: ref}, {Ref: earlier, Alias: scheduler.EarlierLabel}}
	if err != nil || !slices.Equal(jobs, want) {
		t.Errorf("Installed with an earlier job on disk: %+v, %v; want %+v", jobs, err, want)
	}
	// Whatever is defined for an earlier job is readable by its ref.
	if got := s.Definition(site, earlier); !got.Defined || got.DefinitionErr != nil || got.DataHome != own.inst.DataHome {
		t.Errorf("Definition of the earlier job: %+v (%v)", got, got.DefinitionErr)
	}
	// The installation's own job comes first even when only an earlier job
	// has a definition.
	elsewhere := scheduler.Site{UserHome: t.TempDir()}
	earlier = b.Earlier(t, elsewhere, own.inst)
	if jobs, err = s.Installed(ctx, elsewhere, own.inst); err != nil || len(jobs) != 2 || jobs[0] != (scheduler.Job{Ref: ref}) || jobs[1].Ref != earlier {
		t.Errorf("Installed with only an earlier job defined: %+v, %v; want %s first, then %s", jobs, err, ref, earlier)
	}
}

// Locate names the site and the job of a definition path: the inverse of where
// Plan puts an artifact, so a journal that records a path can address the job
// it made at the site it was made at. A path no Plan could have written is
// refused.
func locateInverse(t *testing.T, b Backend) {
	t.Helper()
	s, _, site := fresh(t, b)
	for _, sp := range specimens(t.TempDir()) {
		plan := define(t, s, site, sp)
		for _, artifact := range plan.Artifacts {
			path, _ := artifact.Path()
			got, ref, err := s.Locate(path)
			if err != nil || got != site || ref != plan.Ref {
				t.Errorf("Locate(%s) = %+v, %q, %v; want %+v, %q", path, got, ref, err, site, plan.Ref)
			}
		}
	}
	for _, path := range []string{"", "relative/job", filepath.Join(site.UserHome, "elsewhere", "job"), filepath.Join(site.UserHome, "x") + "/../y"} {
		if _, _, err := s.Locate(path); err == nil {
			t.Errorf("Locate(%q) accepted a path no plan writes", path)
		}
	}
}
