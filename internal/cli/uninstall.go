package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
)

// Uninstall leaves data and credentials available for reinstall unless the
// explicit destructive option and its separate confirmation are supplied.
func runUninstallCommand(args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	if err := uninstall(args, stdin, stdout, env); err != nil {
		fmt.Fprintf(stderr, "Uninstall incomplete: %v\n", err)
		return 1
	}
	return 0
}
func uninstall(args []string, stdin io.Reader, out io.Writer, env Env) error {
	home, err := env.home()
	if err != nil {
		return err
	}
	userHome, err := env.userHomeDir()
	if err != nil {
		return err
	}
	if err = checkRemovableHome(home, userHome); err != nil {
		return err
	}
	if err = os.MkdirAll(home, 0700); err != nil {
		return err
	}
	release, err := local.NamedLock(home, "setup.lock")
	if err != nil {
		return err
	}
	// A purge releases its locks early so it can delete the lock files; every
	// release is idempotent so the deferred calls cannot unlock twice.
	release = releaseOnce(release)
	defer release()
	if transactionPending(home) {
		return fmt.Errorf("run agent-archive setup to recover the interrupted installation first")
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return err
	}
	purge := containsString(args, "--delete-local-data")
	fmt.Fprintln(out, "Remove the archive's hooks and background collector from this Mac. Remote archives are kept.")
	if purge {
		fmt.Fprintf(out, "Also delete owned local state and credentials under %s.\n", home)
	} else {
		fmt.Fprintln(out, "Local evidence, settings, and credentials will be kept. Run setup to reinstall.")
	}
	p := newPrompter(stdin, out)
	yes, err := p.yesNo("Remove integrations?", false)
	if err != nil {
		return err
	}
	if !yes {
		fmt.Fprintln(out, "Cancelled. No changes were made.")
		return nil
	}
	previewPending := 0
	if purge {
		pending, e := pendingSessions(home, config.Config{})
		if e != nil {
			return e
		}
		previewPending = pending
		fmt.Fprintf(out, "%d pending session(s) and all owned local caches will be removed. Unpublished evidence cannot be recovered from the bucket.\n", pending)
		yes, err = p.yesNo("Delete local data and stored credentials too?", false)
		if err != nil {
			return err
		}
		if !yes {
			fmt.Fprintln(out, "Cancelled. No changes were made.")
			return nil
		}
	}
	unlock, err := local.Lock(home)
	if err != nil {
		return fmt.Errorf("another operation is finishing; retry uninstall: %w", err)
	}
	unlock = releaseOnce(unlock)
	defer unlock()
	releaseHooks, err := local.NamedLock(home, "hooks.lock")
	if err != nil {
		return err
	}
	releaseHooks = releaseOnce(releaseHooks)
	defer releaseHooks()
	// Reload after acquiring the lock; pause/resume may have completed meanwhile.
	cfg, found, err = config.Load(home)
	if err != nil {
		return err
	}
	if purge {
		pending, e := pendingSessions(home, config.Config{})
		if e != nil {
			return e
		}
		if pending > previewPending {
			return fmt.Errorf("new pending evidence appeared while confirming; rerun uninstall to review it")
		}
	}
	changes, err := hooks.PlanRemoval(userHome, allHarnesses)
	if err != nil {
		return err
	}
	plist := filepath.Join(userHome, "Library", "LaunchAgents", hooks.LaunchLabel+".plist")
	state := env.jobState(plist)
	if state == "unknown" {
		return fmt.Errorf("cannot determine background job state; restore access to launchctl and retry")
	}
	if state == "running" || state == "loaded" {
		if err = env.unloadLaunchAgent(plist); err != nil {
			return fmt.Errorf("stop collector: %w", err)
		}
	}
	// Disable capture before removing hooks. A partial uninstall remains safely disabled.
	if found {
		cfg.Archive.Enabled = false
		if err = config.Save(home, cfg); err != nil {
			return err
		}
	}
	if err = hooks.Apply(changes); err != nil {
		return err
	}
	if err = os.Remove(plist); err != nil && !os.IsNotExist(err) {
		return err
	}
	if purge {
		refs := map[string]bool{}
		for _, ref := range cfg.RetiredCredentialRefs {
			refs[ref] = true
		}
		if cfg.Storage.R2CredentialRef != "" {
			refs[cfg.Storage.R2CredentialRef] = true
		}
		for _, old := range cfg.PreviousDestinations {
			if old.R2CredentialRef != "" {
				refs[old.R2CredentialRef] = true
			}
		}
		var draft setupDraft
		if e := local.Read(filepath.Join(home, "setup-draft.json"), &draft); e == nil && draft.CredentialRef != "" {
			refs[draft.CredentialRef] = true
			for _, ref := range draft.StagedRefs {
				refs[ref] = true
			}
		} else if e != nil && !os.IsNotExist(e) {
			return e
		}
		// A Keychain that cannot delete an item must not strand the rest of
		// the purge: hooks and the LaunchAgent are already gone, so local
		// files are still removed and the items left behind are named, since
		// once config.json is gone nothing else records them.
		undeleted, keychainErr := deleteCredentialRefs(env, refs)
		leftovers, e := removeLocalState(home)
		if e != nil {
			return e
		}
		// Local state is gone. Unlink the lock files while they are still
		// held, so a hook, collector, or setup that opens one from now on
		// creates a fresh inode it owns outright instead of acquiring this
		// one after its release; then release them and remove the directory
		// itself if nothing unrelated remains in it.
		removeLockFiles(home)
		releaseHooks()
		unlock()
		release()
		if len(leftovers) == 0 {
			if e := os.Remove(home); e != nil && !os.IsNotExist(e) && !isDirectoryNotEmpty(e) {
				return e
			}
		}
		var problems []string
		if len(leftovers) > 0 {
			problems = append(problems, fmt.Sprintf("unrelated files were kept in %s: %s", home, strings.Join(leftovers, ", ")))
		}
		if len(undeleted) > 0 {
			// The account names are opaque random references
			// ("setup-<hex>", see setup.go) that reveal nothing about the
			// stored secret, and after this purge nothing else records them,
			// so they are printed here on purpose (see the PR A3 ledger
			// entry). The recovery is uninstall-specific: there is no
			// configuration left to sync or re-run setup against.
			commands := make([]string, 0, len(undeleted))
			for _, ref := range undeleted {
				commands = append(commands, fmt.Sprintf("security delete-generic-password -s %s -a %s", credentials.KeychainService, ref))
			}
			problem := fmt.Sprintf("%d stored credential(s) could not be deleted from Keychain service %q: %v", len(undeleted), credentials.KeychainService, keychainErr)
			if errors.Is(keychainErr, credentials.ErrKeychainLocked) {
				problem += ". Unlock the login Keychain (log in, or open Keychain Access)"
			}
			problem += fmt.Sprintf(". To remove them yourself, run: %s; or delete those items in Keychain Access", strings.Join(commands, " && "))
			problems = append(problems, problem)
		}
		if len(problems) > 0 {
			return errors.New(strings.Join(problems, "; "))
		}
	}
	fmt.Fprintln(out, "Uninstall complete. Remote archives and the CLI executable were kept.")
	return nil
}

// deleteCredentialRefs deletes every referenced Keychain item it can. It
// returns the references it could not delete, sorted, with the first error;
// an item that is already absent counts as deleted.
func deleteCredentialRefs(env Env, refs map[string]bool) ([]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	sorted := make([]string, 0, len(refs))
	for ref := range refs {
		sorted = append(sorted, ref)
	}
	sort.Strings(sorted)
	kc, err := env.keychain()
	if err != nil {
		return sorted, err
	}
	var undeleted []string
	var firstErr error
	for _, ref := range sorted {
		if e := kc.Delete(context.Background(), ref); e != nil && !errors.Is(e, credentials.ErrMissingCredential) {
			undeleted = append(undeleted, ref)
			if firstErr == nil {
				firstErr = e
			}
		}
	}
	return undeleted, firstErr
}

// uninstallLockFiles are the lock files a purge unlinks after every other
// local entry is gone and while it still holds them: held, they are what
// keeps a hook, collector, or setup from acting on a half-deleted directory,
// and unlinking before release means a later opener gets its own inode.
var uninstallLockFiles = []string{"hooks.lock", "collector.lock", "setup.lock"}

func removeLockFiles(home string) {
	for _, name := range uninstallLockFiles {
		_ = os.Remove(filepath.Join(home, name))
	}
}

func isDirectoryNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}

// releaseOnce makes a lock release safe to call more than once. Calling a
// local.NamedLock release twice would unlock and close its file descriptor
// again, by which time the number may belong to another open file.
func releaseOnce(release func()) func() {
	var once sync.Once
	return func() { once.Do(release) }
}

// checkRemovableHome refuses to touch a data directory that is obviously
// not agent-archive's own: AGENT_ARCHIVE_HOME pointed at the user's home
// directory or a filesystem root. removeLocalState is the second guard: it
// only ever deletes entries agent-archive itself creates.
func checkRemovableHome(home, userHome string) error {
	clean := filepath.Clean(home)
	if clean == filepath.Clean(userHome) || filepath.Dir(clean) == clean {
		return fmt.Errorf("refusing to remove %s: not an agent-archive data directory", home)
	}
	return nil
}

// localStateEntries is every top-level entry agent-archive creates under its
// data directory: internal/config's config.json, collector.LocalStore's
// per-session directories, the lineage ledger, local.Lock's lock file, the
// collector status file, and the LaunchAgent's log files. Keep it in sync
// with those packages; an entry missing here is left behind by uninstall
// (and reported), never silently deleted.
var localStateEntries = []string{
	"config.json", "setup-draft.json", "setup-transaction.json",
	"registrations", "requests", "request-locks", "published", "pending", "sessions", "superseded", "pending-scans", "subagent-candidates",
	"status.json", "storage-health.json", "capture-diagnostics.json", "application-versions.json",
	"collector.lock", "collector.log", "collector-error.log",
}

// removeLocalState deletes agent-archive's own entries under home (see
// localStateEntries, plus local.WriteBytes's ".pending-*" temp files) and
// then the directory itself. It never deletes anything else: a data
// directory a user pointed AGENT_ARCHIVE_HOME at may hold their own files,
// and those are returned as leftover, with the directory left in place,
// rather than removed.
func removeLocalState(home string) (leftover []string, err error) {
	entries, err := os.ReadDir(home)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, name := range localStateEntries {
		known[name] = true
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "setup.lock" || name == "hooks.lock" || name == "collector.lock" {
			continue
		}
		if !known[name] && !strings.HasPrefix(name, ".pending-") {
			leftover = append(leftover, name)
			continue
		}
		if err := os.RemoveAll(filepath.Join(home, name)); err != nil {
			return nil, err
		}
	}
	if len(leftover) > 0 {
		return leftover, nil
	}
	// Lock files are skipped here because they are still held; uninstall
	// removes them after releasing them (see removeLockFiles).
	return nil, nil
}
