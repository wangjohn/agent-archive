package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// runBackfillUndo implements `agent-archive backfill undo [IMPORT_ID] [--project
// DIR] [--yes]`: it removes the sessions an import registered from the
// bucket and from this Mac, and excludes the projects the import added.
//
// setup.lock is held from before the plan until exit, so no setup or other
// backfill changes the configuration while the plan is shown. collector.lock
// is taken after confirming and held until exit, so no collector pass
// republishes a session mid-undo; the selection is then checked again, and
// undo stops if it grew. hooks.lock is held only to write the configuration.
func runBackfillUndo(args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	fail := func(format string, args ...any) int {
		terminal.Printf(stderr, "agent-archive: backfill undo: "+format+"\n", args...)
		return 1
	}
	fs := newCommandFlags("backfill undo", stderr)
	project := fs.String("project", "", "only undo this project's sessions")
	yes := fs.Bool("yes", false, "skip the confirmation")
	// The ID may come before or after the flags.
	id, ok := fs.parseWithArgument(args)
	if !ok {
		return 2
	}

	home, err := env.readHome()
	if err != nil {
		return fail("resolve home: %v", err)
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return fail("load config: %v", err)
	}
	if !found {
		return fail("%s", errNotSetUp.Error())
	}
	if refusal := undoRefusal(home, cfg); refusal != "" {
		return fail("%s", refusal)
	}
	userHome, err := env.userHomeDir()
	if err != nil {
		return fail("resolve user home: %v", err)
	}
	releaseSetup, err := local.NamedLock(home, "setup.lock")
	if err != nil {
		return fail("setup or a backfill is running; run undo again when it finishes. Nothing was changed.")
	}
	defer releaseSetup()

	batches, batch, err := selectUndoBatch(home, id)
	if err != nil {
		return fail("%v", err)
	}
	if batch == nil {
		terminal.Println(stdout, "No imports to undo.")
		return 0
	}
	bfEnv := env.backfillEnvironment(userHome, cfg)
	plan, err := backfill.PlanUndo(bfEnv, state.OpenReadOnly(home), cfg, batches, *batch, *project)
	if err != nil {
		return fail("%v", err)
	}
	// Nothing to do needs no confirmation, so these come before the
	// terminal requirement.
	if plan.Empty() {
		// An earlier run that stopped after changing the configuration left
		// its changes unrecorded; recording them is all that is left.
		if len(plan.Settled.Excluded) > 0 || plan.Settled.RetentionRestored {
			batch.RecordUndone(plan.Settled)
			if err := backfill.SaveBatch(home, *batch); err != nil {
				return fail("%v", err)
			}
		}
		if *project != "" {
			terminal.Printf(stdout, "No sessions from %s are left in import %s. Nothing was changed.\n", plan.ProjectDisplay(), batch.ID)
		} else {
			terminal.Printf(stdout, "Import %s has nothing left to undo. Nothing was changed.\n", batch.ID)
		}
		return 0
	}
	if !*yes && !env.isTerminal(stdin) {
		return fail("confirming an undo needs a terminal. Nothing was changed. Run again with --yes to undo without asking.")
	}
	// The bucket must work before anything is confirmed, as for an import.
	// The check writes one test object and deletes it again.
	var bucket storage.ObjectStore
	if plan.Counts().Deleted > 0 {
		terminal.Print(stdout, "Checking storage… ")
		if bucket, err = env.openStore(cfg); err == nil {
			err = storage.VerifyAccess(context.Background(), bucket)
		}
		if err != nil {
			terminal.Println(stdout, "failed.")
			terminal.Printf(stderr, "agent-archive: backfill undo: storage check failed: %v\n", err)
			if action := credentials.RecoveryAction(err); action != "" {
				terminal.Println(stderr, "agent-archive: backfill undo: "+action)
			}
			return fail("nothing was changed.")
		}
		terminal.Println(stdout, "ready.")
		terminal.Println(stdout)
	}
	backfill.RenderUndo(stdout, plan)
	terminal.Println(stdout)
	if !*yes {
		confirmed, err := newPrompter(stdin, stdout).yesNo(backfill.UndoQuestion(plan), false)
		if err != nil {
			return fail("%v. Nothing was changed.", err)
		}
		if !confirmed {
			terminal.Println(stdout, "Cancelled. Nothing was changed.")
			return 0
		}
	}

	releaseCollector, err := lockCollectorWait(home, "backfill undo", env.now(), backfillCollectorWait)
	if err != nil {
		return fail("a collector pass is still running; run undo again. Nothing was changed.")
	}
	defer releaseCollector()
	// Recheck under the locks: a pass that ran during the prompt can have
	// registered a subagent, or a hook resumed a session.
	fingerprint := configFingerprint(cfg)
	if cfg, found, err = config.Load(home); err != nil {
		return fail("reload config: %v", err)
	}
	if !found {
		return fail("%s", errNotSetUp.Error())
	}
	if refusal := undoRefusal(home, cfg); refusal != "" {
		return fail("%s", refusal)
	}
	if configFingerprint(cfg) != fingerprint {
		return fail("the configuration changed while this was open; run undo again. Nothing was changed.")
	}
	confirmed := plan
	if plan, err = backfill.PlanUndo(bfEnv, state.OpenReadOnly(home), cfg, batches, *batch, *project); err != nil {
		return fail("%v", err)
	}
	if plan.Grew(confirmed) {
		return fail("the import changed while this was open; run undo again to review it. Nothing was changed.")
	}
	if err := checkpoint("undoing"); err != nil {
		return fail("%v", err)
	}

	if plan.Counts().Deleted > 0 && bucket == nil {
		// The recheck found a bucket delete the confirmed plan did not
		// have; Grew refuses that, so this is only a guard.
		return fail("the import changed while this was open; run undo again to review it. Nothing was changed.")
	}
	store, err := state.Open(home)
	if err != nil {
		return fail("open local store: %v", err)
	}
	now := env.now().UTC()
	// The batch is marked undone and the configuration changed before any
	// session is removed: once the projects are excluded, nothing of theirs
	// is published again, even if undo is interrupted, and an import undone
	// in part is never continued. A rerun finishes the sessions.
	changes, err := commitUndo(home, batch, plan, fingerprint, now)
	if err != nil {
		return fail("%v", err)
	}
	if len(changes.Excluded) > 0 || changes.RetentionRestored {
		batch.RecordUndone(changes)
		if err := backfill.SaveBatch(home, *batch); err != nil {
			return fail("%v. The configuration was already changed. Run undo again to remove the sessions.", err)
		}
	}
	result := plan.Remove(context.Background(), store, bucket, now)
	return reportUndo(stdout, stderr, *batch, plan, changes, result)
}

// undoRefusal says why an undo cannot start now, or "". Like an import, it
// waits for a pending setup and for resume: pause suspends remote cleanup.
func undoRefusal(home string, cfg config.Config) string {
	switch {
	case transactionPending(home):
		return "setup needs recovery; run agent-archive setup first. Nothing was changed."
	case cfg.Paused:
		return errPaused.Error() + ". Nothing was changed."
	}
	return ""
}

// selectUndoBatch returns every import and the one named id, or the latest
// when id is empty, or nil when there are no imports. Every batch file must
// be readable: the latest cannot be chosen past one that is not, and which
// projects an undo excludes depends on the others (see backfill.PlanUndo).
func selectUndoBatch(home, id string) ([]backfill.Batch, *backfill.Batch, error) {
	batches, err := backfill.LoadBatches(home)
	if err != nil {
		return nil, nil, fmt.Errorf("%w. Nothing was changed. Repair the file to undo any import", err)
	}
	if id == "" {
		if len(batches) == 0 {
			return nil, nil, nil
		}
		return batches, &batches[len(batches)-1], nil
	}
	for i := range batches {
		if batches[i].ID == id {
			return batches, &batches[i], nil
		}
	}
	return nil, nil, fmt.Errorf("there is no import %q. Run agent-archive backfill history to see import IDs", id)
}

// commitUndo marks the batch undone and then writes the configuration
// change, under hooks.lock, after checking that the configuration is still
// the one the plan was made from. It returns the IDs of the projects it
// excluded and whether it restored retention; the caller records them in the
// batch.
func commitUndo(home string, batch *backfill.Batch, plan backfill.UndoPlan, fingerprint string, now time.Time) (backfill.UndoChanges, error) {
	markUndone := func() error {
		batch.UndoneAt = &now
		batch.RecordKept(plan.KeepProjects)
		batch.RecordUndone(plan.Settled)
		if err := backfill.SaveBatch(home, *batch); err != nil {
			return fmt.Errorf("%w. Nothing was changed", err)
		}
		return nil
	}
	if len(plan.ExcludeProjects) == 0 && len(plan.RemoveApps) == 0 && plan.RestoreRetention == nil && len(plan.RemoveKeptOut) == 0 {
		return backfill.UndoChanges{}, markUndone()
	}
	releaseHooks, err := local.NamedLockWait(home, "hooks.lock", backfillHooksWait)
	if err != nil {
		return backfill.UndoChanges{}, errors.New("capture hooks are busy; run undo again. Nothing was changed")
	}
	defer releaseHooks()
	cfg, found, err := config.Load(home)
	if err != nil {
		return backfill.UndoChanges{}, fmt.Errorf("load config: %w", err)
	}
	if !found {
		return backfill.UndoChanges{}, errNotSetUp
	}
	if configFingerprint(cfg) != fingerprint {
		return backfill.UndoChanges{}, errors.New("the configuration changed while this was open; run undo again. Nothing was changed")
	}
	if err := markUndone(); err != nil {
		return backfill.UndoChanges{}, err
	}
	changes := plan.ApplyToConfig(&cfg)
	if err := config.Save(home, cfg); err != nil {
		return backfill.UndoChanges{}, fmt.Errorf("save config: %w. The import is marked undone but nothing was removed; run undo again", err)
	}
	return changes, nil
}

func reportUndo(stdout, stderr io.Writer, batch backfill.Batch, plan backfill.UndoPlan, changes backfill.UndoChanges, result backfill.UndoResult) int {
	subagents := map[string]bool{}
	for _, s := range plan.Sessions {
		if s.Registration.ParentSessionID != "" {
			subagents[s.Registration.ArchiveSessionID] = true
		}
	}
	split := func(ids []string) (int, int) {
		sessions, subs := 0, 0
		for _, id := range ids {
			if subagents[id] {
				subs++
			} else {
				sessions++
			}
		}
		return sessions, subs
	}
	var parts []string
	if s, a := split(result.Deleted); s+a > 0 {
		parts = append(parts, "deleted "+backfill.SessionsAndSubagents(s, a)+" from the archive")
	}
	if s, a := split(result.Forgotten); s+a > 0 {
		parts = append(parts, "forgot "+backfill.SessionsAndSubagents(s, a)+" from a previous storage destination")
	}
	if len(changes.Excluded) > 0 {
		parts = append(parts, "excluded "+countNoun(len(changes.Excluded), "project"))
	}
	if changes.RetentionRestored && plan.RestoreRetention != nil {
		parts = append(parts, fmt.Sprintf("restored retention to %d days", plan.RestoreRetention.From))
	}
	if len(parts) > 0 {
		line := strings.Join(parts, ", ")
		terminal.Printf(stdout, "Import %s: %s.\n", batch.ID, strings.ToUpper(line[:1])+line[1:])
	}
	if len(result.Failed) == 0 {
		return 0
	}
	ids := make([]string, 0, len(result.Failed))
	for id := range result.Failed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		terminal.Printf(stderr, "agent-archive: backfill undo: session %s: %v\n", id, result.Failed[id])
	}
	terminal.Printf(stderr, "agent-archive: backfill undo: %s could not be removed and %s still registered; run agent-archive backfill undo %s again to finish.\n",
		countNoun(len(ids), "session"), isAre(len(ids)), batch.ID)
	return 1
}
