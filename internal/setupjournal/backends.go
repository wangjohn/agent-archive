package setupjournal

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/scheduler"
)

// DefaultBackend is the scheduler a journal that names none was written for:
// launchd, the only one there was when journals had no backend field.
const DefaultBackend = "launchd"

// Backends resolves the name of a scheduler, as a journal records it, to the
// scheduler that drives its jobs. internal/cli provides it from its Env, so its
// tests' stand-ins apply here too; this package's tests pass their own. Nothing
// in this package runs a scheduler's tool itself, and each job is driven
// through the backend that made it, which is how a setup that moves to another
// backend can still retire the jobs the first one left.
//
// This package trusts an implementation to decide ownership: a job's name
// alone does not prove it is this installation's, so Inspect must compare the
// definition the manager loaded the job from with the one it expects, and
// Unload must stop the job only when they are the same, returning an error
// (and leaving the job running) when they are not or cannot be compared.
type Backends func(name string) (scheduler.Scheduler, error)

// target is one job a journal drives: the backend that runs it, and where
// its definition is. A job the journal cannot resolve (a backend this build
// does not have, a definition path no backend named) is unknown, changes
// nothing, and fails to load or stop with why.
type target struct {
	sched scheduler.Scheduler
	site  scheduler.Site
	ref   scheduler.Ref
	err   error
}

// target resolves the job of a journal or a retired job: backend names the
// scheduler (empty means DefaultBackend, which is how every journal before the
// field was written), and definition is the path of its definition, which says
// where it is (the site) and which job it is. jobRef, when the journal has one,
// must be that same job: a release before the field acts on the job the
// definition names, so a journal whose job_ref names another (only a hand
// edit, or a release that means something else by it, writes one) is refused
// rather than driven two ways, and its job is never mistaken for another
// installation's of the same site.
func (b Backends) target(backend, jobRef, definition string) target {
	name := backend
	if name == "" {
		name = DefaultBackend
	}
	sched, err := b(name)
	if err != nil {
		return target{err: err}
	}
	site, ref, err := sched.Locate(definition)
	if err != nil {
		return target{sched: sched, err: err}
	}
	if jobRef != "" && scheduler.Ref(jobRef) != ref {
		words := sched.Words()
		return target{sched: sched, err: fmt.Errorf("the journal names the %s %s, but its %s %s is the %s %s; %s was left as it is", words.Job, jobRef, words.Definition, definition, words.Job, ref, words.Manager)}
	}
	return target{sched: sched, site: site, ref: ref}
}

// resolve is the collector's job in journal, once every job the journal drives
// through a scheduler resolves (the collector's, and each retired job that was
// loaded): a journal naming one that does not is refused before anything is
// changed, with the job it names and why.
func (b Backends) resolve(journal Journal) (target, error) {
	collector := b.target(journal.Backend, journal.JobRef, journal.Plist)
	if collector.err != nil {
		return collector, fmt.Errorf("the background collector: %w", collector.err)
	}
	for _, job := range journal.retired() {
		if !job.WasLoaded {
			continue
		}
		if t := b.target(job.Backend, job.JobRef, job.Change.Path); t.err != nil {
			return collector, fmt.Errorf("the job %s: %w", job.Change.Path, t.err)
		}
	}
	return collector, nil
}

// state is the job's state, unknown when it cannot be asked.
func (t target) state() scheduler.JobState {
	if t.err != nil {
		return scheduler.Unknown
	}
	return t.sched.Inspect(context.Background(), t.site, t.ref).State
}

func (t target) load() error {
	if t.err != nil {
		return t.err
	}
	return t.sched.Load(context.Background(), t.site, t.ref)
}

func (t target) unload() error {
	if t.err != nil {
		return t.err
	}
	return t.sched.Unload(context.Background(), t.site, t.ref)
}

// removeStranded removes what a job that is no longer defined left of its
// definition: the scheduler's Paths for it, after a rollback deleted its
// files. systemd's enable link is one (it dangles once the unit files are
// gone, and only a `disable` the rollback may not have had to make removes
// it); the scheduler lists only files that are the job's own. A job whose
// definition is back, as the rollback found it, is left as it is.
//
// A path the journal recorded (changes: the definition's own files, which
// setup wrote) is the rollback's alone, and is never removed here: the
// rollback has already put it back as it was, and what it put back, or left
// as setup found it (a link with nothing at its end reads as no file), stays.
// So only what the manager made beside the definition goes, and a scheduler
// that lists only its definition file (launchd) has nothing removed.
//
// It is for a target that resolved: Restore refuses a journal whose
// collector does not before it changes anything.
func (t target) removeStranded(changes []hooks.Change) error {
	status := t.sched.Definition(t.site, t.ref)
	if status.Defined {
		return nil
	}
	recorded := make(map[string]bool, len(changes))
	for _, c := range changes {
		recorded[filepath.Clean(c.Path)] = true
	}
	for _, path := range status.Paths {
		if recorded[filepath.Clean(path)] {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// tool is the command that drives the job's scheduler, for messages.
func (t target) tool() string {
	if t.sched == nil {
		return "the scheduler"
	}
	return t.sched.Words().Tool
}

// blocked is a recovery stopped because the tool failed to do what it was
// asked, which rerunning setup alone may not change either.
func (t target) blocked(home, action string, err error) error {
	return &RecoveryBlockedError{home: home, cause: fmt.Sprintf("%s could not %s (%v); once %s works again, rerun setup", t.tool(), action, err, t.tool())}
}
