package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
)

type setupJournal struct {
	Legacy    *legacyJob     `json:"legacy,omitempty"`
	Changes   []hooks.Change `json:"changes"`
	Plist     string         `json:"plist"`
	WasLoaded bool           `json:"was_loaded"`
}

func journalPath(home string) string { return filepath.Join(home, "setup-transaction.json") }

func transactionPending(home string) bool {
	_, err := os.Stat(journalPath(home))
	return !os.IsNotExist(err)
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

// pendingSessions counts every accepted session with work outstanding, a
// session waiting for its transcript included: status reports it as pending,
// because from the user's side it is.
func pendingSessions(home string, cfg config.Config) (int, error) {
	blocking, waiting, err := pendingSessionCounts(home, cfg)
	return blocking + waiting, err
}

// pendingSessionCounts splits the pending sessions in two. waiting counts
// registrations with no transcript path that have never published and have
// no publication in flight: a Cursor chat whose transcript never arrived
// (transcripts turned off, for example). Nothing of such a session can be
// published anywhere until a path arrives, so its queued request is not work
// a sync could finish. blocking counts everything else.
func pendingSessionCounts(home string, cfg config.Config) (blocking, waiting int, err error) {
	store := collector.OpenLocalStoreReadOnly(home)

	regs, err := store.LoadRegistrations()
	if err != nil {
		return 0, 0, err
	}
	reqs, err := store.LoadRequests()
	if err != nil {
		return 0, 0, err
	}
	requested := map[string]bool{}
	for _, r := range reqs {
		requested[r.ArchiveSessionID] = true
	}
	for _, r := range regs {
		if !cfg.AcceptSession(r) {
			continue
		}
		_, _, state, found, err := store.LoadPublished(r.ArchiveSessionID)
		if err != nil {
			return 0, 0, err
		}
		scanPending, err := store.ScanPending(r.ArchiveSessionID)
		if err != nil {
			return 0, 0, err
		}
		if !scanPending && !requested[r.ArchiveSessionID] && found && state != collector.CacheStatusRateLimited {
			continue
		}
		idle, err := waitingForTranscript(store, r)
		if err != nil {
			return 0, 0, err
		}
		if idle {
			waiting++
		} else {
			blocking++
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
	regs, err := collector.OpenLocalStoreReadOnly(home).LoadRegistrations()
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

// waitingForTranscript reports whether a registration has no transcript path,
// has never published, and has no publication in flight. A Cursor database
// chat has no transcript path by design and never waits for one: it is
// read from the database, so a sync can publish it and it is pending.
func waitingForTranscript(store *collector.LocalStore, r archive.SessionRegistration) (bool, error) {
	if r.TranscriptPath != "" || !r.ReadsTranscriptFile() {
		return false, nil
	}
	_, _, published, err := store.LoadLastPublished(r.ArchiveSessionID)
	if err != nil || published {
		return false, err
	}
	pending, err := store.HasPending(r.ArchiveSessionID)
	return !pending, err
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
		store := collector.OpenLocalStoreReadOnly(home)
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
	unlock, err := local.Lock(home)
	if err != nil {
		return fmt.Errorf("another operation is running; retry setup when it finishes: %w", err)
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
	changes, err := hooks.Plan(userHome, executable, next.Harnesses)
	if err != nil {
		return err
	}
	var removed []string
	for _, app := range old.Harnesses {
		if !containsString(next.Harnesses, app) {
			removed = append(removed, app)
		}
	}
	removals, err := hooks.PlanRemoval(userHome, removed)
	if err != nil {
		return err
	}
	changes = append(changes, removals...)
	plistPath := filepath.Join(userHome, "Library", "LaunchAgents", hooks.LaunchLabel+".plist")
	plist, err := hooks.LaunchAgent(executable, home)
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
	if job == "unknown" && old.MachineID != "" {
		return fmt.Errorf("cannot determine previous background job state; restore access to launchctl and retry")
	}
	legacy, err := planLegacyMigration(userHome, env)
	if err != nil {
		return err
	}
	journal := setupJournal{Legacy: legacy, Changes: changes, Plist: plistPath, WasLoaded: launchJobActive(job)}
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
func launchJobActive(state string) bool {
	//lint:ignore LV1001 Env.JobState (cli.go) reports launchd states as plain strings, and tests stub it with string-returning funcs
	return state == "loaded" || state == "running"
}

// Recover only files still equal to our before/after snapshots. A user's later
// edits are never overwritten by crash recovery.
func restoreSetup(home string, journal setupJournal, env Env) error {
	state := env.jobState(journal.Plist)
	if launchJobActive(state) {
		if err := env.unloadLaunchAgent(journal.Plist); err != nil {
			return err
		}
	} else if state == "unknown" {
		return fmt.Errorf("background job state is unknown; recovery cannot safely continue")
	}
	var changed []hooks.Change
	for _, c := range journal.Changes {
		b, err := os.ReadFile(c.Path)
		if (os.IsNotExist(err) && !c.Existed) || (err == nil && string(b) == string(c.Before) && c.Existed) {
			continue
		}
		if err != nil || string(b) != string(c.After) {
			return fmt.Errorf("%s changed outside setup; preserve it and resolve the transaction before retrying", c.Path)
		}
		changed = append(changed, c)
	}
	if err := hooks.Rollback(changed); err != nil {
		return err
	}
	if journal.WasLoaded {
		if err := env.loadLaunchAgent(journal.Plist); err != nil {
			return err
		}
	}
	if err := restoreLegacyJob(journal.Legacy, env); err != nil {
		return err
	}
	return os.Remove(journalPath(home))
}

func recoverSetup(home string, env Env) error {
	var journal setupJournal
	err := local.Read(journalPath(home), &journal)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	unlock, err := local.Lock(home)
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
