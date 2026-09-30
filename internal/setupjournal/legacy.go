package setupjournal

import (
	"bytes"
	"fmt"
	"os"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/scheduler"
)

// LegacyJob is a job a setup retires: the prototype's upload job, or a
// collector an earlier release installed under another label. Change holds
// its plist as setup found it.
type LegacyJob struct {
	Change    hooks.Change `json:"change"`
	WasLoaded bool         `json:"was_loaded"`
	// Backend and JobRef say which scheduler runs the job and what it calls
	// it, as Journal's do for the collector; both are optional, and absent
	// means DefaultBackend and the job Change.Path names.
	Backend string `json:"backend,omitempty"`
	JobRef  string `json:"job_ref,omitempty"`
}

// RetireeJobs is the journal's record of the jobs setup retires: the
// prototype's job (at most one: the journal has a field of its own for it, as
// every release has written it) and the collectors under earlier labels, in
// order. Each job's definition is a single file, as found, which is what the
// journal restores.
func RetireeJobs(retirees []scheduler.Retiree) (legacy *LegacyJob, relabeled []*LegacyJob, err error) {
	for _, r := range retirees {
		if len(r.Artifacts) != 1 {
			return nil, nil, fmt.Errorf("cannot record the %s job %s: it has %d definitions, and the journal records one file", r.Backend, r.Ref, len(r.Artifacts))
		}
		path, ok := r.Artifacts[0].Path()
		if !ok {
			return nil, nil, fmt.Errorf("cannot record the %s job %s: %s is not a file", r.Backend, r.Ref, r.Artifacts[0].ID)
		}
		job := &LegacyJob{Change: hooks.Change{Path: path, Before: r.Artifacts[0].After, Existed: true, Mode: r.Artifacts[0].Mode}, WasLoaded: r.WasLoaded, Backend: r.Backend, JobRef: string(r.Ref)}
		if r.Alias == scheduler.Prototype && legacy == nil {
			legacy = job
			continue
		}
		relabeled = append(relabeled, job)
	}
	return legacy, relabeled, nil
}

func retireLegacyJob(job *LegacyJob, backends Backends) error {
	if job == nil {
		return nil
	}
	current, err := os.ReadFile(job.Change.Path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, job.Change.Before) {
		return fmt.Errorf("legacy upload job changed during setup; retry")
	}
	if job.WasLoaded {
		if err := backends.target(job.Backend, job.JobRef, job.Change.Path).unload(); err != nil {
			return err
		}
	}
	return os.Remove(job.Change.Path)
}

// restoreLegacyJob puts back a job setup retired: the legacy upload job, or
// a collector an earlier release installed under another label. name
// says which in errors, and home is the data directory whose interrupted
// setup is being recovered.
func restoreLegacyJob(home string, job *LegacyJob, name string, backends Backends) error {
	if job == nil {
		return nil
	}
	if err := checkLegacyJob(home, job, name); err != nil {
		return err
	}
	if _, err := os.Stat(job.Change.Path); os.IsNotExist(err) {
		if err := hooks.Apply([]hooks.Change{{Path: job.Change.Path, After: job.Change.Before, Mode: job.Change.Mode}}); err != nil {
			return err
		}
	}
	if job.WasLoaded {
		target := backends.target(job.Backend, job.JobRef, job.Change.Path)
		switch target.state() {
		case scheduler.Unknown:
			return &RecoveryBlockedError{home: home, cause: fmt.Sprintf("the state of the %s is unknown; restore access to %s and rerun setup", name, target.tool())}
		case scheduler.AnotherInstallation, scheduler.Loaded, scheduler.Running:
			// Running already; or launchd runs the label from another
			// installation's plist now, a job that is not this one's and
			// that launchd refuses to bootstrap over, so the plist is back
			// and that job is left alone, as Restore leaves the collector's.
			// Stopping recovery there would only keep the record until
			// --abandon-recovery, with the jobs after this one not put
			// back; setup, run again, plans from what launchd runs then.
		case scheduler.Missing:
			if err := target.load(); err != nil {
				return target.blocked(home, "restart the "+name, err)
			}
		}
	}
	return nil
}

// checkLegacyJob confirms restoreLegacyJob can put job back: its plist is
// either gone (setup removed it) or exactly as setup found it.
func checkLegacyJob(home string, job *LegacyJob, name string) error {
	if job == nil {
		return nil
	}
	current, err := os.ReadFile(job.Change.Path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(current, job.Change.Before) {
		return &RecoveryBlockedError{home: home, cause: fmt.Sprintf("the %s's plist %s changed outside setup, and recovery never overwrites your edits", name, job.Change.Path)}
	}
	return nil
}
