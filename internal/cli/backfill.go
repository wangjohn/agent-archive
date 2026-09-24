package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }
func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

// runBackfillCommand implements `agent-archive backfill`: it finds the
// sessions already on this Mac, shows the plan, and after confirmation
// imports them (see docs/agent-archive-backfill-spec.md). `--dry-run
// [--json]` prints the plan and writes nothing, locally or remotely.
func runBackfillCommand(args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	if len(args) > 0 && args[0] == "history" {
		return runBackfillHistory(args[1:], stdout, stderr, env)
	}
	if len(args) > 0 && args[0] == "undo" {
		return runBackfillUndo(args[1:], stdin, stdout, stderr, env)
	}
	fs := flag.NewFlagSet("backfill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var harnesses, projects stringList
	fs.Var(&harnesses, "harness", "only sessions from this app (claude, codex, cursor); repeatable")
	fs.Var(&projects, "project", "only sessions in this project directory; repeatable")
	since := fs.String("since", "", "only sessions started on or after this local date (YYYY-MM-DD)")
	until := fs.String("until", "", "only sessions started on or before this local date (YYYY-MM-DD)")
	includeHome := fs.Bool("include-home", false, "import sessions run from the home directory")
	includeTemp := fs.Bool("include-temp", false, "import sessions run from temporary directories")
	includeRemoved := fs.Bool("include-removed", false, "import sessions retention or undo removed")
	dryRun := fs.Bool("dry-run", false, "print the plan and exit; nothing is written")
	jsonOut := fs.Bool("json", false, "with --dry-run, print the plan as JSON")
	yes := fs.Bool("yes", false, "skip the confirmation")
	background := fs.Bool("background", false, "register the sessions and let the collector upload them")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	usageError := func(message string) int {
		fmt.Fprintf(stderr, "agent-archive: backfill: %s\n", message)
		return 2
	}
	if fs.NArg() != 0 {
		return usageError(fmt.Sprintf("unexpected argument %q", fs.Arg(0)))
	}
	filters := backfill.Filters{
		Harnesses: harnesses, Projects: projects, Since: *since, Until: *until,
		IncludeHome: *includeHome, IncludeTemp: *includeTemp, IncludeRemoved: *includeRemoved,
	}
	if err := filters.Validate(); err != nil {
		return usageError(err.Error())
	}
	if *jsonOut && !*dryRun {
		return usageError("--json applies only to --dry-run")
	}
	if *dryRun && *background {
		return usageError("--background applies only to an import, not --dry-run")
	}

	home, err := env.readHome()
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: backfill: resolve home: %v\n", err)
		return 1
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: backfill: load config: %v\n", err)
		return 1
	}
	if !found {
		fmt.Fprintln(stderr, "agent-archive: backfill: "+errNotSetUp.Error())
		return 1
	}
	// A dry run works while paused or while a setup transaction is pending:
	// it writes nothing. An import refuses both before it looks at anything.
	if !*dryRun {
		if refusal := importRefusal(home, cfg); refusal != "" {
			fmt.Fprintln(stderr, "agent-archive: backfill: "+refusal)
			return 1
		}
		if !*yes && !env.isTerminal(stdin) {
			fmt.Fprintln(stderr, "agent-archive: backfill: confirming an import needs a terminal. Nothing was changed. Run again with --yes to import without asking, or with --dry-run to see the plan.")
			return 1
		}
	}
	userHome, err := env.userHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: backfill: resolve user home: %v\n", err)
		return 1
	}

	if !*jsonOut {
		fmt.Fprint(stdout, backfill.SearchLine(filters)+" ")
	}
	plan, err := backfill.BuildPlan(context.Background(), env.backfillEnvironment(userHome), newArchiveState(home, cfg), cfg, filters)
	if err != nil {
		if !*jsonOut {
			fmt.Fprintln(stdout)
		}
		fmt.Fprintf(stderr, "agent-archive: backfill: %v\n", err)
		return 1
	}
	if *jsonOut {
		if err := backfill.RenderJSON(stdout, plan, false); err != nil {
			fmt.Fprintf(stderr, "agent-archive: backfill: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "%d found.\n", plan.Found())
	if *dryRun {
		fmt.Fprintln(stdout)
		backfill.RenderText(stdout, plan)
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "Dry run: nothing was changed.")
		return 0
	}
	if len(plan.Imported()) == 0 {
		fmt.Fprintln(stdout)
		backfill.RenderText(stdout, plan)
		if err := finishInterruptedBatch(env, stdout, home, plan, cfg); err != nil {
			fmt.Fprintf(stderr, "agent-archive: backfill: %v\n", err)
			return 1
		}
		return 0
	}

	// Step 2: storage must work before anything is confirmed. The check
	// writes one test object and deletes it again.
	fmt.Fprint(stdout, "Checking storage… ")
	if err := checkStorage(env, cfg); err != nil {
		fmt.Fprintln(stdout, "failed.")
		fmt.Fprintf(stderr, "agent-archive: backfill: storage check failed: %v\n", err)
		if action := credentials.RecoveryAction(err); action != "" {
			fmt.Fprintln(stderr, "agent-archive: backfill: "+action)
		}
		fmt.Fprintln(stderr, "agent-archive: backfill: nothing was imported.")
		return 1
	}
	fmt.Fprintln(stdout, "ready.")
	fmt.Fprintln(stdout)
	backfill.RenderText(stdout, plan)
	fmt.Fprintln(stdout)

	// Step 3: confirm. edit changes the retention of the whole archive and
	// shows the plan again with the new deletion date.
	if !*yes {
		confirmed, err := confirmImport(newPrompter(stdin, stdout), stdout, &plan)
		if err != nil {
			fmt.Fprintf(stderr, "agent-archive: backfill: %v. Nothing was changed.\n", err)
			return 1
		}
		if !confirmed {
			fmt.Fprintln(stdout, "Cancelled. Nothing was changed.")
			return 0
		}
	}
	return importPlan(env, stdout, stderr, home, plan, configFingerprint(cfg), *background)
}

// importRefusal says why an import cannot start now, or "".
func importRefusal(home string, cfg config.Config) string {
	switch {
	case transactionPending(home):
		return "setup needs recovery; run agent-archive setup first. Nothing was changed."
	case cfg.Paused:
		return errPaused.Error() + ". Nothing was changed."
	case !cfg.Archive.Enabled:
		return "integrations are not installed; run agent-archive setup to reinstall. Nothing was changed."
	}
	return ""
}

// checkStorage runs the setup round trip against the configured bucket.
func checkStorage(env Env, cfg config.Config) error {
	store, err := env.openStore(cfg)
	if err != nil {
		return err
	}
	return storage.VerifyAccess(context.Background(), store)
}

// confirmImport asks `Import N sessions from M projects? [y/N/edit]`. The
// default is No. edit asks for a new retention period, which it stores in
// plan, and shows the plan again.
func confirmImport(p *prompter, out io.Writer, plan *backfill.Plan) (bool, error) {
	for {
		answer, err := p.line(fmt.Sprintf("Import %s from %s? [y/N/edit] ", countNoun(len(plan.Imported()), "session"), countNoun(len(plan.Projects()), "project")))
		if err != nil {
			return false, err
		}
		switch strings.ToLower(answer) {
		case "", "n", "no":
			return false, nil
		case "y", "yes":
			return true, nil
		case "e", "edit":
			days := plan.RetentionDays
			if days <= 0 {
				days = defaultRetentionDays
			}
			fmt.Fprintln(out, "Retention applies to every session in the archive, not only these.")
			if plan.RetentionDays, err = p.intWithDefault("Keep sessions for how many days?", days); err != nil {
				return false, err
			}
			fmt.Fprintln(out)
			backfill.RenderText(out, *plan)
			fmt.Fprintln(out)
		default:
			fmt.Fprintln(out, "Please enter y, n, or edit.")
		}
	}
}

func countNoun(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// configFingerprint identifies the configuration a plan was made from. The
// collector refreshes bucket privacy evidence in place, which is evidence
// rather than a setting, so it is left out, as setup leaves it out when it
// checks for concurrent changes.
func configFingerprint(cfg config.Config) string {
	data, _ := json.Marshal(withoutBucketPrivacy(cfg))
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// backfillEnvironment is what planning reads: the user's home, the
// temporary directories, and the clock. Files are read from the real file
// system.
func (e Env) backfillEnvironment(userHome string) backfill.Environment {
	temps := e.BackfillTempDirs
	if temps == nil {
		temps = append([]string(nil), backfill.DefaultTempDirs...)
		if tmp, ok := e.lookupEnv("TMPDIR"); ok && strings.TrimSpace(tmp) != "" {
			temps = append(temps, strings.TrimSpace(tmp))
		}
	}
	return backfill.Environment{Home: userHome, TempDirs: temps, Now: e.now}
}

// archiveState answers backfill.ArchiveState from this machine's local store
// and configuration.
type archiveState struct {
	store *collector.LocalStore
	cfg   config.Config
}

func newArchiveState(home string, cfg config.Config) archiveState {
	return archiveState{store: collector.OpenLocalStoreReadOnly(home), cfg: cfg}
}

// Classify reports already_archived when the native session has a
// registration the configuration accepts, and registered_not_admitted when it
// has one the configuration no longer accepts. An index entry without a
// registration does not count, as for hooks (hasRegistration).
func (s archiveState) Classify(harness, nativeSessionID string) (backfill.SkipReason, error) {
	archiveID, found, err := s.store.ArchiveSessionID(nativeSessionID)
	if err != nil {
		return "", err
	}
	if found {
		reg, found, err := s.store.LoadRegistration(archiveID)
		if err != nil {
			return "", err
		}
		if found {
			if s.cfg.AcceptSession(reg) {
				return backfill.SkipAlreadyArchived, nil
			}
			return backfill.SkipRegisteredNotAdmitted, nil
		}
	}
	// Retention and undo leave a removal record when they forget a session,
	// so a transcript still on disk is not imported again.
	record, found, err := s.store.Removal(harness, nativeSessionID)
	if err != nil || !found {
		return "", err
	}
	if record.Reason == collector.RemovalReasonUndo {
		return backfill.SkipRemovedByUndo, nil
	}
	return backfill.SkipRemovedByRetention, nil
}
