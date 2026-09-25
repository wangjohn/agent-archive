package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/evidence"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/retention"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

var (
	errNotSetUp = errors.New("not set up yet; run `agent-archive setup` first")
	errPaused   = errors.New("collection is paused; run `agent-archive resume` first")
)

// collector-error.log is cut back to its last errorLogKeepBytes once it
// grows past errorLogMaxBytes.
const (
	errorLogMaxBytes  = 1 << 20
	errorLogKeepBytes = 256 << 10
)

// Deadlines for one pass. A pass holds the collector lock, and every
// scheduled tick that finds it held gives up quietly, so a pass that never
// ended would stop capture without a word.
//
// Collection has two. Past collectSoftDeadline the pass starts no new
// session, as when backfill is interrupted: the session in flight finishes
// and the rest keep their work for the next pass. collectHardDeadline cuts
// off the session in flight too, and is set well above the time the largest
// source a session can have (archive.MaxRecordBytes, 64 MiB) takes over a
// slow uplink, so one big upload is not cut off and restarted on every pass.
// A session cut off this way is not reported as failing. The retention sweep
// gets its own budget, so a slow collection cannot starve cleanup.
//
// Variables only so tests can shorten them.
var (
	collectSoftDeadline = 10 * time.Minute
	collectHardDeadline = 60 * time.Minute
	sweepTimeout        = 5 * time.Minute
)

// runCollectCommand implements the hidden `_collect` entry point
// install.LaunchAgent schedules every 60 seconds. Unlike `sync`, it never
// reports "already running" as a problem: a scheduled tick finding the
// previous one still working is the lock doing its job, not an error.
func runCollectCommand(_ []string, _ io.Writer, stderr io.Writer, env Env) int {
	// launchd appends this process's stderr to collector-error.log and never
	// rotates it. launchd runs one _collect at a time, so this process is the
	// file's only writer until it exits.
	if home, err := env.home(); err == nil {
		_ = local.TrimLog(filepath.Join(home, "collector-error.log"), errorLogMaxBytes, errorLogKeepBytes)
	}
	_, err := runOnePass(env, true)
	if err != nil {
		if errors.Is(err, errNotSetUp) || errors.Is(err, errPaused) {
			return 0
		}
		terminal.Printf(stderr, "agent-archive: collect: %v\n", err)
		return 1
	}
	return 0
}

// runOnePass loads configuration, honors pause, takes the machine lock, and
// runs one collector.Run pass. quietOnBusy controls whether a contended lock
// is reported as an error or treated as an expected, silent no-op.
//
// Once localStore exists, any failure before collector.Run gets its own
// chance to record Status is written into that same Status's LastError.
// Without this, a broken lock or bad storage credentials would fail every
// scheduled _collect tick while `status` kept reporting the last successful
// scan's LastError (typically empty), leaving a misconfigured install
// looking healthy.
func runOnePass(env Env, quietOnBusy bool) (collector.Result, error) {
	return runPass(env, quietOnBusy, passOptions{})
}

// passOptions are what a caller watching one pass passes through to
// collector.Run: backfill's upload progress and Ctrl-C.
type passOptions struct {
	progress func(collector.Progress)
	stop     func() bool
}

func runPass(env Env, quietOnBusy bool, pass passOptions) (collector.Result, error) {
	home, err := env.home()
	if err != nil {
		return collector.Result{}, fmt.Errorf("resolve home: %w", err)
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return collector.Result{}, fmt.Errorf("load config: %w", err)
	}
	if !found {
		return collector.Result{}, errNotSetUp
	}
	if cfg.Paused {
		return collector.Result{}, errPaused
	}

	localStore, err := state.Open(home)
	if err != nil {
		return collector.Result{}, fmt.Errorf("open local store: %w", err)
	}

	holder := "sync"
	switch {
	case pass.progress != nil:
		holder = "backfill upload"
	case quietOnBusy:
		holder = "scheduled collection"
	}
	unlock, err := lockCollector(home, holder, env.now())
	if err != nil {
		if errors.Is(err, local.ErrBusy) {
			if quietOnBusy {
				return collector.Result{}, nil
			}
			return collector.Result{}, err
		}
		// Not recorded into Status here: without the lock, a concurrent
		// holder's own SaveStatus (from collector.Run or this same
		// function) could race an unguarded read-modify-write to
		// status.json and lose an update. Every failure below this point
		// runs only after the lock is held, so it can record safely.
		return collector.Result{}, fmt.Errorf("acquire lock: %w", err)
	}
	defer unlock()
	pruneHandoffs(home, env.now())
	if transactionPending(home) {
		return collector.Result{}, fmt.Errorf("setup needs recovery; run agent-archive setup")
	}
	cfg, found, err = config.Load(home)
	if err != nil {
		return collector.Result{}, err
	}
	if !found || !cfg.Archive.Enabled {
		return collector.Result{}, errNotSetUp
	}
	if cfg.Paused {
		return collector.Result{}, errPaused
	}

	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), collectHardDeadline)
	defer cancel()
	stop := func() bool {
		return time.Since(started) >= collectSoftDeadline || (pass.stop != nil && pass.stop())
	}
	objectStore, err := env.openStore(cfg)
	if err != nil {
		storeErr := fmt.Errorf("open storage: %w", err)
		recordPreflightError(localStore, storeErr)
		_ = recordStorageHealth(home, cfg, env, quietOnBusy, "credentials_unavailable")
		return collector.Result{}, storeErr
	}
	if quietOnBusy {
		var prior storageHealth
		healthErr := local.Read(filepath.Join(home, "storage-health.json"), &prior)
		if healthErr != nil || prior.ConfigurationID != configurationID(cfg) || prior.Context != "background_collector" || prior.State != "verified" || env.now().Sub(prior.CheckedAt) > storageHealthRefreshAfter {
			probeErr := storage.VerifyAccess(ctx, objectStore)
			state := "verified"
			if probeErr != nil {
				state = storageFailureState(probeErr)
			}
			if err := recordStorageHealth(home, cfg, env, true, state); err != nil {
				return collector.Result{}, err
			}
			if probeErr != nil {
				recordPreflightError(localStore, fmt.Errorf("background storage access failed; restore credentials or connectivity and retry"))
				return collector.Result{}, probeErr
			}
		}
		// Privacy evidence is persisted in config.json exactly as setup saves
		// it, so status and setup review read one source. The config was
		// re-read under the lock above, so the save cannot lose another
		// writer's update.
		if bucketPrivacyNeedsRefresh(cfg, env.now()) {
			cfg.BucketPrivacy = inspectBucketPrivacy(cfg, objectStore, env.now())
			if err := config.Save(home, cfg); err != nil {
				return collector.Result{}, fmt.Errorf("save bucket privacy evidence: %w", err)
			}
		}
	}

	result, err := collector.Run(ctx, localStore, objectStore, collector.Options{
		MachineID:            cfg.MachineID,
		SupplementalEvidence: skillObserver(env),
		AcceptSession:        cfg.AcceptSession,
		Now:                  env.Now,
		RequireSkillUse:      cfg.RequireSkillUse,
		Progress:             pass.progress,
		Stop:                 stop,
		CursorDatabase:       env.cursorDatabase(),
	})
	if err != nil {
		return result, err
	}

	// A read-back verification failure is reported, but only once the
	// retention sweep below has run: it is no reason to skip cleanup.
	var verifyErr error
	if _, err := verifyPublicationsWithin(ctx, home, cfg, env, localStore, objectStore); err != nil {
		verifyErr = fmt.Errorf("read-back verification: %w", err)
	}
	if health := passStorageHealth(result); health != "not_checked" {
		if err := recordStorageHealth(home, cfg, env, quietOnBusy, health); err != nil {
			return result, err
		}
	}
	if len(result.Errors) > 0 {
		current, readErr := localStore.LoadStatus()
		if readErr != nil {
			return result, readErr
		}
		current.SessionIssues = map[string]string{}
		for id, issue := range result.Errors {
			code := "capture_or_publication_failed"
			switch {
			case errors.Is(issue, state.ErrQuarantined):
				code = "local_state_unreadable"
			case strings.Contains(issue.Error(), "truncated, compacted, or rewritten"):
				code = "transcript_discontinuity"
			case strings.Contains(issue.Error(), "collection limit"):
				code = "transcript_size_limit"
			}
			current.SessionIssues[id] = code
		}
		if err := localStore.SaveStatus(current); err != nil {
			return result, err
		}
		recordPreflightError(localStore, fmt.Errorf("%d session(s) need capture or publication", len(result.Errors)))
	}

	sweepCtx, cancelSweep := context.WithTimeout(context.Background(), sweepTimeout)
	defer cancelSweep()
	sweepResult, sweepErr := retention.Sweep(sweepCtx, localStore, objectStore, retention.Options{
		Now: env.Now,
		// Retention sweeps every session this machine registered, including
		// ones cfg.AcceptSession no longer admits for publication: an excluded
		// project's already-published sessions still own objects in this
		// bucket and must age out of it. Only a session admitted into another
		// destination published somewhere else, and for those the sweep
		// prunes local state without touching this bucket. That is decided by
		// the registration's destination ID, or for an older registration
		// without one by its admission (not its start) against
		// DestinationSince.
		CurrentDestination: cfg.InCurrentDestination,
		// Outstanding work defers expiry only when the collector will do it.
		Publishable:   cfg.AcceptSession,
		SessionMaxAge: time.Duration(cfg.RetentionDays) * 24 * time.Hour,
	})
	if sweepErr != nil {
		passErr := errors.Join(verifyErr, fmt.Errorf("collection succeeded but retention cleanup failed: %w", sweepErr))
		recordPreflightError(localStore, passErr)
		return result, passErr
	}
	if len(sweepResult.Errors) > 0 {
		recordRetentionErrors(localStore, &result, sweepResult)
	}
	if verifyErr != nil {
		recordPreflightError(localStore, verifyErr)
		return result, verifyErr
	}
	return result, nil
}

// passStorageHealth derives the storage-access evidence one pass produced. A
// publication is a real round trip, so it stands as "verified" even when
// other sessions failed for reasons unrelated to storage (a rewritten
// transcript, a size limit). Only a genuine storage error can downgrade it:
// an authentication failure always wins, and a plain storage outage leaves
// the pass unchecked only when nothing was published through it.
func passStorageHealth(result collector.Result) string {
	health := "not_checked"
	if len(result.Published) > 0 {
		health = "verified"
	}
	for _, sessionErr := range result.Errors {
		if !isStorageError(sessionErr) {
			continue
		}
		if state := storageFailureState(sessionErr); state != "storage_unavailable" {
			return state
		}
		if len(result.Published) == 0 {
			health = "not_checked"
		}
	}
	return health
}

// recordRetentionErrors merges retention.Sweep's per-session failures into
// result, so sync's existing report/exit-code logic (which only knows about
// collector.Result) covers them too without its own retention-specific
// path, and updates Status.LastError the same way collector.Run already
// does for its own per-session errors — otherwise a retention failure would
// never reach `status` at all, since, unlike collector.Run, Sweep does not
// persist a Status of its own.
func recordRetentionErrors(localStore *state.Store, result *collector.Result, sweep retention.Result) {
	if result.Errors == nil {
		result.Errors = map[string]error{}
	}
	for id, sweepErr := range sweep.Errors {
		result.Errors[id] = fmt.Errorf("retention: %w", sweepErr)
	}
	status, err := localStore.LoadStatus()
	if err != nil {
		return
	}
	status.LastError = fmt.Sprintf("%d session(s) failed to scan, publish, or clean up", len(result.Errors))
	_ = localStore.SaveStatus(status)
}

// recordPreflightError persists a failure that happened before collector.Run
// could record its own Status, so `status` reflects it. Best-effort: if the
// status write itself fails, the original error is still what the caller
// returns and reports.
func recordPreflightError(localStore *state.Store, preflightErr error) {
	status, err := localStore.LoadStatus()
	if err != nil {
		return
	}
	status.LastError = preflightErr.Error()
	_ = localStore.SaveStatus(status)
}

// openConfiguredStore resolves cfg.Storage into a live ObjectStore. A
// Keychain being unavailable (a non-darwin build, or cgo disabled) is only
// fatal if the configured provider is R2 and therefore actually needs it;
// storage.NewConfiguredStore surfaces that.
func openConfiguredStore(cfg config.Config) (storage.ObjectStore, error) {
	keychain, keychainErr := credentials.NewKeychainStore(credentials.KeychainService)
	if keychainErr != nil && cfg.Storage.Provider == credentials.ProviderR2 {
		return nil, fmt.Errorf("keychain unavailable: %w", keychainErr)
	}
	return storage.NewConfiguredStore(context.Background(), cfg.Storage, keychain)
}

// skillObserver shares bounded observations within a pass: user-scope skill
// roots are read once per harness, project-scope roots once per
// harness/project. Each read carries its own instruction-content cap, so a
// pass reads at most that cap per harness plus that cap per project.
//
// An imported session gets no observation: today's skills attached to a
// session that ran before them would be false evidence.
func skillObserver(env Env) func(archive.SessionRegistration, time.Time) ([]archive.SupplementalEvidence, error) {
	userCache := map[string][]archive.SupplementalEvidence{}
	projectCache := map[string][]archive.SupplementalEvidence{}
	return func(reg archive.SessionRegistration, at time.Time) ([]archive.SupplementalEvidence, error) {
		if reg.Imported() {
			return nil, nil
		}
		userScope, ok := userCache[reg.Harness.Name]
		if !ok {
			userHome, err := env.userHomeDir()
			if err != nil {
				return nil, err
			}
			userScope, err = evidence.ObserveSkills(evidence.SkillOptions{Harness: reg.Harness.Name, UserHome: userHome, ObservedAt: at})
			if err != nil {
				return nil, err
			}
			userCache[reg.Harness.Name] = userScope
		}
		projectKey := reg.Harness.Name + "\x00" + reg.ProjectRoot
		projectScope, ok := projectCache[projectKey]
		if !ok {
			var err error
			projectScope, err = evidence.ObserveSkills(evidence.SkillOptions{Harness: reg.Harness.Name, ProjectRoot: reg.ProjectRoot, ObservedAt: at})
			if err != nil {
				return nil, err
			}
			projectCache[projectKey] = projectScope
		}
		return append(append([]archive.SupplementalEvidence(nil), userScope...), projectScope...), nil
	}
}
