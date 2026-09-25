// Package setupjournal is setup's transaction record: the journal setup
// writes before it changes any hook file or the LaunchAgent, which recovery
// reads to put an interrupted setup back, and which every other command
// checks to see that one is pending.
package setupjournal

import (
	"os"
	"path/filepath"
)

// JournalPath is where setup records its transaction in the data directory home.
func JournalPath(home string) string { return filepath.Join(home, "setup-transaction.json") }

// TransactionPending reports whether an interrupted or running setup's
// journal exists (or cannot be checked) in home.
func TransactionPending(home string) bool {
	_, err := os.Stat(JournalPath(home))
	return !os.IsNotExist(err)
}

type setupJournal struct {
	Legacy *legacyJob `json:"legacy,omitempty"`
	// Relabeled and MoreRelabeled are the collectors earlier releases
	// installed for this data directory under other labels (see
	// previousCollectorPlists); setup retires them in favor of the
	// directory's own label. The first stays in Relabeled, where releases
	// that retired at most one recorded it, so either reads the other's
	// journal of one.
	Relabeled     *legacyJob     `json:"relabeled,omitempty"`
	MoreRelabeled []*legacyJob   `json:"more_relabeled,omitempty"`
	Changes       []hooks.Change `json:"changes"`
	Plist         string         `json:"plist"`
	WasLoaded     bool           `json:"was_loaded"`
}

// relabeled lists every collector the journal retires under another label.
func (j setupJournal) relabeled() []*legacyJob {
	if j.Relabeled == nil {
		return j.MoreRelabeled
	}
	return append([]*legacyJob{j.Relabeled}, j.MoreRelabeled...)
}

// commitSetup records journal, then makes the changes it holds: it stops the
// collector when it was loaded, applies every file change, retires the jobs
// the journal retires, and starts the collector. A failure on the way puts
// everything back from the journal (restoreSetup); success removes it.
func commitSetup(home string, journal setupJournal, env Env) error {
	if err := local.Write(setupjournal.JournalPath(home), journal); err != nil {
		return err
	}
	fail := func(cause error) error {
		if rb := restoreSetup(home, journal, env); rb != nil {
			return errors.Join(cause, fmt.Errorf("rollback incomplete; run setup again: %w", rb))
		}
		return fmt.Errorf("previous installation restored: %w", cause)
	}
	if journal.WasLoaded {
		if err := env.unloadLaunchAgent(journal.Plist); err != nil {
			return fail(fmt.Errorf("stop previous collector: %w", err))
		}
	}
	if err := hooks.Apply(journal.Changes); err != nil {
		return fail(err)
	}
	if err := retireLegacyJob(journal.Legacy, env); err != nil {
		return fail(err)
	}
	for _, job := range journal.relabeled() {
		if err := retireLegacyJob(job, env); err != nil {
			return fail(fmt.Errorf("retire the %s: %w", relabeledJobName, err))
		}
	}
	if err := env.loadLaunchAgent(journal.Plist); err != nil {
		return fail(fmt.Errorf("start background collector: %w", err))
	}
	if err := os.Remove(setupjournal.JournalPath(home)); err != nil {
		return fail(err)
	}
	return nil
}

// Recover only files still equal to our before/after snapshots. A user's later
// edits are never overwritten by crash recovery.
func restoreSetup(home string, journal setupJournal, env Env) error {
	// Every file is checked before anything, the collector included, is
	// touched: a recovery that stops halfway would leave less to go on.
	var changed []hooks.Change
	for _, c := range journal.Changes {
		if c.Unapplied() {
			continue
		}
		if !c.Applied() {
			return &recoveryBlockedError{home: home, cause: c.Path + " changed outside setup, and recovery never overwrites your edits"}
		}
		changed = append(changed, c)
	}
	if err := checkLegacyJob(home, journal.Legacy, legacyJobName); err != nil {
		return err
	}
	for _, job := range journal.relabeled() {
		if err := checkLegacyJob(home, job, relabeledJobName); err != nil {
			return err
		}
	}
	state := env.jobState(journal.Plist)
	if launchJobActive(state) {
		if err := env.unloadLaunchAgent(journal.Plist); err != nil {
			return launchctlBlocked(home, "stop the background collector", err)
		}
	} else if state == "unknown" {
		return &recoveryBlockedError{home: home, cause: "the background collector's state is unknown, so recovery cannot safely continue; restore access to launchctl and rerun setup"}
	}
	if err := hooks.Rollback(changed); err != nil {
		return &recoveryBlockedError{home: home, cause: fmt.Sprintf("the files setup changed could not all be put back (%v)", err)}
	}
	if journal.WasLoaded {
		if err := env.loadLaunchAgent(journal.Plist); err != nil {
			return launchctlBlocked(home, "restart the background collector", err)
		}
	}
	if err := restoreLegacyJob(home, journal.Legacy, legacyJobName, env); err != nil {
		return err
	}
	for _, job := range journal.relabeled() {
		if err := restoreLegacyJob(home, job, relabeledJobName, env); err != nil {
			return err
		}
	}
	return os.Remove(setupjournal.JournalPath(home))
}

// Names of the jobs a setup journal can retire, as recovery errors call them.
const (
	legacyJobName    = "legacy upload job"
	relabeledJobName = "background collector installed under an earlier label"
)

// recoveryBlockedError is a recovery that cannot proceed without the user:
// a file changed outside setup, or launchd cannot be asked. Rerunning setup
// alone would stop at the same place, so it carries the way out.
type recoveryBlockedError struct {
	home  string
	cause string
}

func (e *recoveryBlockedError) Error() string {
	return "cannot recover the interrupted setup: " + e.cause
}

// launchctlBlocked is a recovery stopped because launchctl failed to do what
// it was asked, which rerunning setup alone may not change either.
func launchctlBlocked(home, action string, err error) error {
	return &recoveryBlockedError{home: home, cause: fmt.Sprintf("launchctl could not %s (%v); once launchctl works again, rerun setup", action, err)}
}

func (e *recoveryBlockedError) guidance() string {
	return fmt.Sprintf("The interrupted setup is recorded in %s.\nTo keep every file as it is now and discard that record, run: agent-archive setup --abandon-recovery\nThen run agent-archive setup to review your settings.", setupjournal.JournalPath(e.home))
}

// recoverJournal puts back the setup an interrupted journal in home records,
// if there is one. It reads the journal first, then takes the collector lock
// (lockCollector) and hooks.lock, so nothing runs a pass or a hook while
// files are restored.
func recoverJournal(home string, env Env, lockCollector func() (func(), error)) error {
	var journal setupJournal
	err := local.Read(setupjournal.JournalPath(home), &journal)
	if os.IsNotExist(err) {
		return nil
	}
	if state.IsUndecodable(err) {
		return &recoveryBlockedError{home: home, cause: fmt.Sprintf("its record %s could not be read (%v), so nothing in it can be put back", setupjournal.JournalPath(home), err)}
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", setupjournal.JournalPath(home), err)
	}
	unlock, err := lockCollector()
	if err != nil {
		return err
	}
	defer unlock()
	releaseHooks, err := local.NamedLock(home, "hooks.lock")
	if err != nil {
		return err
	}
	defer releaseHooks()
	return restoreSetup(home, journal, env)
}

// planRelabel prepares retiring the collectors earlier releases installed
// for this data directory under other labels: plists, from
// previousCollectorPlists.
func planRelabel(plists []string, env Env) ([]*legacyJob, error) {
	var jobs []*legacyJob
	for _, path := range plists {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		state := env.jobState(path)
		if state == "unknown" {
			return nil, fmt.Errorf("cannot determine the state of %s; restore access to launchctl and retry", path)
		}
		if state == jobAnotherInstallation {
			// launchd runs that label from another plist: not this one's to retire.
			continue
		}
		jobs = append(jobs, &legacyJob{Change: hooks.Change{Path: path, Before: data, Existed: true, Mode: info.Mode().Perm()}, WasLoaded: launchJobActive(state)})
	}
	return jobs, nil
}
