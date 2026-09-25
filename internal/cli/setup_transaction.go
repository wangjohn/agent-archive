package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

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

func discardDraft(home string, draft setupDraft, active config.Config, env Env) error {
	refs := append([]string{}, draft.StagedRefs...)
	if draft.CredentialRef != "" && !containsString(refs, draft.CredentialRef) {
		refs = append(refs, draft.CredentialRef)
	}
	for _, ref := range refs {
		if ref == active.Storage.R2CredentialRef {
			continue
		}
		kc, err := env.keychain()
		if err != nil {
			return err
		}
		if err = kc.Delete(context.Background(), ref); err != nil && !errors.Is(err, credentials.ErrMissingCredential) {
			return err
		}
	}

	err := os.Remove(filepath.Join(home, "setup-draft.json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// destinationEqual reports whether two storage configurations are the same
// destination. It compares destination IDs, so a destination change is
// exactly a change of the ID registrations record.
func destinationEqual(a, b credentials.Config) bool {
	return config.DestinationID(a) == config.DestinationID(b)
}

// pendingSessionCounts splits the pending sessions in two. waiting counts
// those only waiting for their transcript (state.Outstanding's
// WaitingForTranscript): nothing of such a session can be published
// anywhere until a path arrives, so its queued request is not work a sync
// could finish. blocking counts everything else.
func pendingSessionCounts(home string, cfg config.Config) (blocking, waiting int, err error) {
	store := state.OpenReadOnly(home)
	regs, err := store.LoadRegistrations()
	if err != nil {
		return 0, 0, err
	}
	reqs, err := store.LoadRequests()
	if err != nil {
		return 0, 0, err
	}
	queued := state.QueuedRequests(reqs)
	for _, r := range regs {
		if !cfg.AcceptSession(r) {
			continue
		}
		owed, err := store.Outstanding(r, queued[r.ArchiveSessionID])
		if err != nil {
			return 0, 0, err
		}
		switch {
		case !owed.Pending():
		case owed.SyncCanFinish():
			blocking++
		default:
			waiting++
		}
	}
	return blocking, waiting, nil
}

// sessionsAdmittedInto counts the registrations that record cfg's
// destination and that cfg accepts: when setup switches back to a
// destination used before, these resume there. Subagents go with their
// parents and are not counted. A registration without a destination ID
// stays behind.
func sessionsAdmittedInto(home string, cfg config.Config) (int, error) {
	regs, err := state.OpenReadOnly(home).LoadRegistrations()
	if err != nil {
		return 0, err
	}
	id, count := cfg.DestinationID(), 0
	for _, r := range regs {
		if r.ParentSessionID == "" && r.DestinationID == id && cfg.AcceptSession(r) {
			count++
		}
	}
	return count, nil
}

func reviewChanges(home string, old, next config.Config, p *prompter, env Env) error {
	if old.MachineID == "" {
		return nil
	}
	if !destinationEqual(old.Storage, next.Storage) {
		// A session still waiting for its transcript has nothing a sync could
		// publish, so it must not hold the user at the old destination.
		pending, waiting, err := pendingSessionCounts(home, old)
		if err != nil {
			return err
		}
		if pending > 0 {
			return fmt.Errorf("%d session(s) still pending at the current destination; run agent-archive sync before changing storage", pending)
		}
		returning, err := sessionsAdmittedInto(home, next)
		if err != nil {
			return err
		}
		p.warn("Storage is changing. Sessions already archived stay at the old destination,",
			"and this Mac stops adding to or cleaning up there. Nothing is deleted from either bucket;",
			"this Mac's local copies are removed once they pass the retention period.")
		if returning > 0 {
			p.warn(fmt.Sprintf("%d session(s) from when this destination was used before resume uploading there,", returning),
				"and are deleted from it once they pass the retention period.")
		}
		if waiting > 0 {
			p.warn(fmt.Sprintf("%d session(s) never received a transcript (for example a Cursor chat with transcripts turned off).", waiting),
				"They published nothing and will not be captured at the new destination either.")
		}
	}
	if next.RetentionDays < old.RetentionDays {
		store := state.OpenReadOnly(home)
		regs, err := store.LoadRegistrations()
		if err != nil {
			return err
		}
		cutoff := env.now().Add(-time.Duration(next.RetentionDays) * 24 * time.Hour)
		count := 0
		for _, r := range regs {
			if !old.AcceptSession(r) {
				continue
			}
			bundle, _, _, found, err := store.LoadPublished(r.ArchiveSessionID)
			if err != nil {
				return fmt.Errorf("cannot preview retention effect: %w", err)
			}
			if found && !bundle.Capture.CapturedAt.IsZero() && !bundle.Capture.CapturedAt.After(cutoff) {
				count++
			}
		}
		p.warn(fmt.Sprintf("Shorter retention: %d session(s) captured before %s become eligible for deletion.", count, cutoff.UTC().Format("2006-01-02 15:04 UTC")),
			"Future cleanup also applies this policy.")
	}
	return nil
}

func fileChange(path string, after []byte) (hooks.Change, error) {
	before, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return hooks.Change{}, err
	}
	return hooks.Change{Path: path, Before: before, After: after, Existed: err == nil, Mode: 0600}, nil
}

// carriedImportedHarnesses is the ImportedHarnesses a setup commits. Backfill
// writes the list, so it comes from the committed configuration, never a
// draft. An app whose hooks setup now installs moves to Harnesses, where it
// admits its imports too, and an app the person chose to stop publishing
// imports for (stopImported) is dropped.
func carriedImportedHarnesses(committed, harnesses, stopImported []string) []string {
	var out []string
	for _, app := range committed {
		if !containsString(harnesses, app) && !containsString(stopImported, app) {
			out = append(out, app)
		}
	}
	return out
}

func applySetup(home, userHome, executable string, old config.Config, next *config.Config, stopImported []string, env Env) error {
	unlock, err := lockCollector(home, "setup", env.now())
	if err != nil {
		return fmt.Errorf("%s holds the collector lock; retry setup when it finishes: %w", lockHolder(home), err)
	}
	defer unlock()
	releaseHooks, err := local.NamedLock(home, "hooks.lock")
	if err != nil {
		return err
	}
	defer releaseHooks()
	current, _, err := config.Load(home)
	if err != nil {
		return err
	}
	// The background collector refreshes bucket privacy evidence in place
	// while setup is open. It is evidence, not a setting, so it neither counts
	// as a concurrent change nor gets overwritten by an older draft report.
	if !reflect.DeepEqual(withoutBucketPrivacy(current), withoutBucketPrivacy(old)) {
		return fmt.Errorf("settings changed while setup was open; restart setup to review the current settings")
	}
	if fresher := freshestBucketPrivacy(*next, current.BucketPrivacy); fresher != nil {
		next.BucketPrivacy = fresher
	}
	// Operational ownership comes from committed state, never a resumable
	// draft. A crash after commit can leave a pre-commit draft on disk.
	next.DestinationSince = old.DestinationSince
	next.PreviousDestinations = append([]credentials.Config(nil), old.PreviousDestinations...)
	next.ImportedHarnesses = carriedImportedHarnesses(old.ImportedHarnesses, next.Harnesses, stopImported)
	for _, ref := range old.RetiredCredentialRefs {
		if !containsString(next.RetiredCredentialRefs, ref) {
			next.RetiredCredentialRefs = append(next.RetiredCredentialRefs, ref)
		}
	}
	if old.MachineID != "" && !destinationEqual(old.Storage, next.Storage) {
		// The same rule as reviewChanges: a session waiting for its
		// transcript does not block the change, and like any unpublished
		// session it falls behind the new DestinationSince.
		pending, _, err := pendingSessionCounts(home, old)
		if err != nil {
			return err
		}
		if pending > 0 {
			return fmt.Errorf("new pending work appeared at the old destination; sync it before retrying")
		}
		next.DestinationSince = env.now().UTC()
		next.PreviousDestinations = append(next.PreviousDestinations, old.Storage)
	}
	if old.Storage.R2CredentialRef != "" && old.Storage.R2CredentialRef != next.Storage.R2CredentialRef {
		next.RetiredCredentialRefs = append(next.RetiredCredentialRefs, old.Storage.R2CredentialRef)
	}
	// Only a project this setup includes is checked: one that was already
	// included may be a folder backfill imported that no longer exists, and
	// it must not block every later change to the setup.
	alreadyIncluded := map[string]bool{}
	for _, project := range old.Archive.Projects {
		if project.Included {
			alreadyIncluded[project.Root] = true
		}
	}
	for _, project := range next.Archive.Projects {
		if project.Included && !alreadyIncluded[project.Root] {
			info, e := os.Stat(project.Root)
			if e != nil || !info.IsDir() {
				return fmt.Errorf("included project is no longer a directory: %s", project.Root)
			}
		}
	}
	next.MachineID = old.MachineID
	if next.MachineID == "" {
		next.MachineID, err = local.ID()
		if err != nil {
			return err
		}
	}
	next.SchemaVersion = config.SchemaVersion
	next.Paused = old.Paused
	next.Archive.Enabled = true
	next.Archive.MachineID = next.MachineID
	next.Archive.SchemaVersion = 1
	for i := range next.Archive.Projects {
		for _, prior := range old.Archive.Projects {
			if prior.Root == next.Archive.Projects[i].Root {
				next.Archive.Projects[i].ActivatedAt = prior.ActivatedAt
				break
			}
		}
		if next.Archive.Projects[i].ActivatedAt.IsZero() {
			next.Archive.Projects[i].ActivatedAt = env.now().UTC()
		}
	}
	// Record the exact path the hooks and LaunchAgent are about to run, so
	// status checks them against it rather than against whatever path status
	// was later started through. It is written with the same transaction.
	next.InstalledExecutable = executable
	// Hooks go where the apps read them in the environment setup runs in;
	// the paths are recorded so later commands find them without it.
	files := env.hookFiles(userHome)
	previousFiles := env.installedHookFiles(userHome, old)
	next.HookFiles = map[string]string{}
	for _, app := range next.Harnesses {
		next.HookFiles[app] = files[app]
	}
	// Another installation's hooks in a file this one would install into
	// mean every session would be captured twice; they are its to remove.
	if problems := env.installation(home, userHome).otherInstallationProblems(files, next.Harnesses); len(problems) > 0 {
		return &otherInstallationError{problems: problems}
	}
	changes, err := hooks.Plan(files, env.installation(home, userHome).hook(executable), next.Harnesses)
	if err != nil {
		return err
	}
	// Remove our hooks from apps no longer selected, and from an app's
	// previous file when its configuration directory has moved.
	for _, app := range old.Harnesses {
		if containsString(next.Harnesses, app) && previousFiles[app] == files[app] {
			continue
		}
		removal, found, err := hooks.PlanRemovalOf(previousFiles, env.installation(home, userHome).owner(), app)
		if err != nil {
			return err
		}
		if found {
			changes = append(changes, removal)
		}
	}
	plistPath := env.installation(home, userHome).collectorPlist()
	plist, err := hooks.LaunchAgent(executable, home, launchLabel(plistPath))
	if err != nil {
		return err
	}
	change, err := fileChange(plistPath, plist)
	if err != nil {
		return err
	}
	changes = append(changes, change)
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	change, err = fileChange(filepath.Join(home, "config.json"), append(data, '\n'))
	if err != nil {
		return err
	}
	changes = append(changes, change)
	job := env.jobState(plistPath)
	// Unknown refuses even a first setup: loading over a job launchd may
	// already run under this label is the one thing setup must not do.
	if job == "unknown" {
		return fmt.Errorf("cannot determine the background job's state; restore access to launchctl and retry")
	}
	if job == jobAnotherInstallation {
		return fmt.Errorf("launchd's %s job was loaded from a plist other than %s, so it belongs to another installation; setup leaves it running and installs nothing over it. Uninstall that installation first, or set AGENT_ARCHIVE_HOME to a directory of this installation's own", launchLabel(plistPath), plistPath)
	}
	// The prototype's job is the account's, retired only by the account's
	// default installation: a test installation must not change it.
	var legacy *legacyJob
	if env.installation(home, userHome).isDefault() {
		if legacy, err = planLegacyMigration(userHome, env); err != nil {
			return err
		}
	}
	relabeled, err := planRelabel(home, userHome, env)
	if err != nil {
		return err
	}
	var firstRelabeled *legacyJob
	var moreRelabeled []*legacyJob
	if len(relabeled) > 0 {
		firstRelabeled, moreRelabeled = relabeled[0], relabeled[1:]
	}
	journal := setupJournal{Legacy: legacy, Relabeled: firstRelabeled, MoreRelabeled: moreRelabeled, Changes: changes, Plist: plistPath, WasLoaded: launchJobActive(job)}
	if err = local.Write(journalPath(home), journal); err != nil {
		return err
	}
	fail := func(cause error) error {
		if rb := restoreSetup(home, journal, env); rb != nil {
			return errors.Join(cause, fmt.Errorf("rollback incomplete; run setup again: %w", rb))
		}
		return fmt.Errorf("previous installation restored: %w", cause)
	}
	if journal.WasLoaded {
		if err = env.unloadLaunchAgent(plistPath); err != nil {
			return fail(fmt.Errorf("stop previous collector: %w", err))
		}
	}
	if err = hooks.Apply(changes); err != nil {
		return fail(err)
	}
	if err = retireLegacyJob(journal.Legacy, env); err != nil {
		return fail(err)
	}
	for _, job := range journal.relabeled() {
		if err = retireLegacyJob(job, env); err != nil {
			return fail(fmt.Errorf("retire the %s: %w", relabeledJobName, err))
		}
	}
	if err = env.loadLaunchAgent(plistPath); err != nil {
		return fail(fmt.Errorf("start background collector: %w", err))
	}
	if err = os.Remove(journalPath(home)); err != nil {
		return fail(err)
	}
	return nil
}

// launchJobActive reports whether a launchd job state from Env.JobState means
// the job is loaded, whether or not it is running at the moment.
func launchJobActive(job string) bool {
	//lint:ignore LV1001 Env.JobState (cli.go) reports launchd states as plain strings, and tests stub it with string-returning funcs
	return job == "loaded" || job == "running"
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
	return os.Remove(journalPath(home))
}

// Names of the jobs a setup journal can retire, as recovery errors call them.
const (
	legacyJobName    = "legacy upload job"
	relabeledJobName = "background collector installed under an earlier label"
)

// otherInstallationError is a setup refused because another installation's
// hooks are in a hook file it would install into (see
// describeOtherInstallations). Rerunning setup stops there again until they
// are gone, which only the user can decide.
type otherInstallationError struct{ problems []string }

func (e *otherInstallationError) Error() string { return strings.Join(e.problems, "\n") }

func (e *otherInstallationError) guidance() string {
	return "Nothing was installed; your answers are saved. Once the other installation's hooks are gone (or this installation has its own HOME), run agent-archive setup to continue."
}

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
	return fmt.Sprintf("The interrupted setup is recorded in %s.\nTo keep every file as it is now and discard that record, run: agent-archive setup --abandon-recovery\nThen run agent-archive setup to review your settings.", journalPath(e.home))
}

// recoveryPending is what a command other than setup says while an
// interrupted setup's record exists: where the record is, and both ways
// out.
func recoveryPending(home string) string {
	return fmt.Sprintf("setup was interrupted and needs recovery (recorded in %s). Run agent-archive setup to recover it; if setup reports a file changed outside setup, run agent-archive setup --abandon-recovery to keep your files as they are now", journalPath(home))
}

// abandonRecovery discards an interrupted setup's record without touching
// any file it lists: hook files, the LaunchAgent, and settings all stay as
// they are now, which is the way out when recovery refuses to overwrite a
// file edited since (see restoreSetup). Setup afterwards reviews and
// reinstalls from there.
func abandonRecovery(out io.Writer, env Env) error {
	home, err := env.home()
	if err != nil {
		return err
	}
	release, err := local.NamedLock(home, "setup.lock")
	if err != nil {
		return err
	}
	defer release()
	unlock, err := lockCollector(home, "setup", env.now())
	if err != nil {
		return fmt.Errorf("%s holds the collector lock; retry when it finishes: %w", lockHolder(home), err)
	}
	defer unlock()
	releaseHooks, err := local.NamedLock(home, "hooks.lock")
	if err != nil {
		return err
	}
	defer releaseHooks()
	var journal setupJournal
	err = local.Read(journalPath(home), &journal)
	if os.IsNotExist(err) {
		terminal.Println(out, "No interrupted setup to discard. Nothing was changed.")
		return nil
	}
	if state.IsUndecodable(err) {
		// Nothing in it can be trusted, so nothing in it is acted on; it is
		// kept for anyone who wants to see what setup was doing.
		aside, moveErr := moveAside(journalPath(home))
		if moveErr != nil {
			return moveErr
		}
		terminal.Printf(out, "The interrupted setup's record %s could not be read (%v). It was moved to %s, and every file was kept as it is now.\n", journalPath(home), err, aside)
		terminal.Println(out, "Next: run agent-archive setup to review your settings; it reinstalls the hooks and starts the background collector again.")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", journalPath(home), err)
	}
	if err = os.Remove(journalPath(home)); err != nil {
		return err
	}
	terminal.Printf(out, "Discarded the interrupted setup recorded in %s. These files were kept as they are now:\n", journalPath(home))
	for _, c := range journal.Changes {
		terminal.Printf(out, "  %s\n", c.Path)
	}
	terminal.Println(out, "The background collector may be stopped: the interrupted setup can have stopped it before it was interrupted, and nothing restarts it now.")
	terminal.Println(out, "Next: run agent-archive setup to review your settings; it reinstalls the hooks and starts the background collector again. agent-archive status shows what is running.")
	return nil
}

func recoverSetup(home string, env Env) error {
	var journal setupJournal
	err := local.Read(journalPath(home), &journal)
	if os.IsNotExist(err) {
		return nil
	}
	if state.IsUndecodable(err) {
		return &recoveryBlockedError{home: home, cause: fmt.Sprintf("its record %s could not be read (%v), so nothing in it can be put back", journalPath(home), err)}
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", journalPath(home), err)
	}
	unlock, err := lockCollector(home, "setup", env.now())
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

// planRelabel prepares retiring every collector earlier releases installed
// for home under other labels (previousCollectorPlists).
func planRelabel(home, userHome string, env Env) ([]*legacyJob, error) {
	var jobs []*legacyJob
	for _, path := range env.installation(home, userHome).previousCollectorPlists() {
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

func withoutBucketPrivacy(cfg config.Config) config.Config {
	cfg.BucketPrivacy = nil
	return cfg
}

// freshestBucketPrivacy returns the newer of cfg's own report and candidate
// when candidate was checked for cfg's storage configuration, otherwise nil.
func freshestBucketPrivacy(cfg config.Config, candidate *storage.PrivacyReport) *storage.PrivacyReport {
	if candidate == nil || candidate.CheckedAt == nil || candidate.ConfigurationID != privacyConfigurationID(cfg) {
		return nil
	}
	if own := cfg.BucketPrivacy; own != nil && own.CheckedAt != nil && own.ConfigurationID == candidate.ConfigurationID && !own.CheckedAt.Before(*candidate.CheckedAt) {
		return nil
	}
	return candidate
}
