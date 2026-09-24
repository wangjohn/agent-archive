package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
)

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }
func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

// runBackfillCommand implements `agent-archive backfill`. This version plans
// only: `--dry-run [--json]` prints what an import would do and writes
// nothing, locally or remotely.
func runBackfillCommand(args []string, stdout, stderr io.Writer, env Env) int {
	if len(args) > 0 && (args[0] == "history" || args[0] == "undo") {
		fmt.Fprintf(stderr, "agent-archive: backfill %s is not available yet\n", args[0])
		return 1
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
	// --yes and --background are accepted so scripts can be written against
	// the final interface; they take effect once import exists.
	fs.Bool("yes", false, "skip the confirmation")
	fs.Bool("background", false, "register the sessions and let the collector upload them")
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
	// it writes nothing.
	if !*dryRun {
		fmt.Fprintln(stderr, "agent-archive: backfill import is not available yet; run agent-archive backfill --dry-run to see the plan")
		return 1
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
	fmt.Fprintf(stdout, "%d found.\n\n", plan.Found())
	backfill.RenderText(stdout, plan)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Dry run: nothing was changed.")
	return 0
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
