package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

// backfillDay is the local day (YYYY-MM-DD) a --since or --until value
// names, in the forms parseTimeArg reads, or "" for "".
func backfillDay(value string, now time.Time) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	t, err := parseTimeArg(value, now, now.Location())
	if err != nil {
		return "", err
	}
	return t.In(now.Location()).Format("2006-01-02"), nil
}

// relativeTimeArg is a --since or --until value as typed when it is
// relative to now (an age: 30d, 12h), which names a different local day
// each day, or "" for a date, an RFC 3339 time, or nothing.
func relativeTimeArg(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if _, err := time.Parse(time.RFC3339, value); err == nil {
		return ""
	}
	if _, err := time.Parse("2006-01-02", value); err == nil {
		return ""
	}
	return value
}

// printInterruptedImport points at the latest import when it was
// interrupted and this run, with other options or another destination,
// would not continue it: the run starts a new import, and the interrupted
// one stays partial unless it is run again as it was.
func printInterruptedImport(out io.Writer, home string, plan backfill.Plan, cfg config.Config) {
	batches, err := backfill.LoadBatches(home)
	if err != nil || len(batches) == 0 {
		return
	}
	last := batches[len(batches)-1]
	if last.CompletedAt != nil || last.UndoneAt != nil || last.Matches(plan.BatchFilters(), cfg.DestinationID()) {
		return
	}
	if last.DestinationID != cfg.DestinationID() {
		terminal.Printf(out, "Import %s was interrupted, for a storage destination no longer configured;\nthis run starts a new import.\n", last.ID)
		return
	}
	flags, ok := last.Filters.Flags(plan, func(id string) (string, bool) {
		for _, p := range cfg.Archive.Projects {
			if p.ProjectID == id {
				return p.Root, true
			}
		}
		return "", false
	})
	if !ok {
		terminal.Printf(out, "Import %s was interrupted; this run, with other options, starts a new import.\n", last.ID)
		return
	}
	command := strings.TrimSpace("agent-archive backfill " + flags)
	terminal.Printf(out, "Import %s was interrupted; this run, with other options, starts a new\nimport. To finish %s instead, run:\n  %s\n", last.ID, last.ID, command)
}

// runBackfillCommand implements `agent-archive backfill`: it finds the
// sessions already on this Mac, shows the plan, and after confirmation
// imports them (see docs/design/backfill.md). `--dry-run
// [--json]` prints the plan and writes nothing, locally or remotely.
func runBackfillCommand(args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	// A copy of Cursor's database a killed backfill or collector left in the
	// temporary folder goes first, whatever this command is (history, undo,
	// and --dry-run included). A copy in use is never removed.
	cursorstore.RemoveStaleSnapshots()
	if len(args) > 0 && args[0] == "history" {
		return runBackfillHistory(args[1:], stdout, stderr, env)
	}
	if len(args) > 0 && args[0] == "undo" {
		return runBackfillUndo(args[1:], stdin, stdout, stderr, env)
	}
	fs := newCommandFlags("backfill", stderr)
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
	if !fs.parseFlagsOnly(args) {
		return 2
	}
	usageError := func(message string) int { return fs.usageError("%s", message) }
	// Backfill selects whole local days, so --since and --until accept the
	// same forms as list's --since and name the local day they fall on.
	sinceDay, err := backfillDay(*since, env.now())
	if err != nil {
		return usageError("--since: " + err.Error())
	}
	untilDay, err := backfillDay(*until, env.now())
	if err != nil {
		return usageError("--until: " + err.Error())
	}
	filters := backfill.Filters{
		Harnesses: harnesses, Projects: projects, Since: sinceDay, Until: untilDay,
		SinceArg: relativeTimeArg(*since), UntilArg: relativeTimeArg(*until),
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
		terminal.Printf(stderr, "agent-archive: backfill: resolve home: %v\n", err)
		return 1
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: backfill: load config: %v\n", err)
		return 1
	}
	if !found {
		terminal.Println(stderr, "agent-archive: backfill: "+errNotSetUp.Error())
		return 1
	}
	// A dry run works while paused or while a setup transaction is pending:
	// it writes nothing. An import refuses both before it looks at anything.
	if !*dryRun {
		if refusal := importRefusal(home, cfg); refusal != "" {
			terminal.Println(stderr, "agent-archive: backfill: "+refusal)
			return 1
		}
		if !*yes && !env.isTerminal(stdin) {
			terminal.Println(stderr, "agent-archive: backfill: confirming an import needs a terminal. Nothing was changed. Run again with --yes to import without asking, or with --dry-run to see the plan.")
			return 1
		}
	}
	userHome, err := env.userHomeDir()
	if err != nil {
		terminal.Printf(stderr, "agent-archive: backfill: resolve user home: %v\n", err)
		return 1
	}

	if !*jsonOut {
		terminal.Print(stdout, backfill.SearchLine(filters)+" ")
	}
	// Ctrl-C during planning cancels it, so the plan's copy of Cursor's
	// database is removed on the way out instead of left in the temporary
	// folder. A second Ctrl-C, SIGTERM, or SIGHUP quits at once, removing
	// the copy first.
	planCtx, stopPlanning := interruptibleContext(env, stderr)
	plan, err := backfill.BuildPlan(planCtx, env.backfillEnvironment(userHome), newArchiveState(home, cfg), cfg, filters)
	interrupted := planCtx.Err() != nil
	stopPlanning()
	if err != nil {
		if !*jsonOut {
			terminal.Println(stdout)
		}
		if interrupted {
			terminal.Println(stderr, "agent-archive: backfill: stopped. Nothing was changed.")
			return 1
		}
		terminal.Printf(stderr, "agent-archive: backfill: %v\n", err)
		return 1
	}
	if *jsonOut {
		if err := backfill.RenderJSON(stdout, plan, false); err != nil {
			terminal.Printf(stderr, "agent-archive: backfill: %v\n", err)
			return 1
		}
		return 0
	}
	terminal.Printf(stdout, "%d found.\n", plan.Found())
	if *dryRun {
		terminal.Println(stdout)
		backfill.RenderText(stdout, plan)
		terminal.Println(stdout)
		printInterruptedImport(stdout, home, plan, cfg)
		terminal.Println(stdout, "Dry run: nothing was changed.")
		return 0
	}
	if len(plan.Imported()) == 0 {
		terminal.Println(stdout)
		backfill.RenderText(stdout, plan)
		printInterruptedImport(stdout, home, plan, cfg)
		if err := finishInterruptedBatch(env, stdout, home, plan, cfg); err != nil {
			terminal.Printf(stderr, "agent-archive: backfill: %v\n", err)
			return 1
		}
		return 0
	}

	// Step 2: storage must work before anything is confirmed. The check
	// writes one test object and deletes it again.
	terminal.Print(stdout, "Checking storage… ")
	if err := checkStorage(env, cfg); err != nil {
		terminal.Println(stdout, "failed.")
		terminal.Printf(stderr, "agent-archive: backfill: storage check failed: %v\n", err)
		if action := credentials.RecoveryAction(err); action != "" {
			terminal.Println(stderr, "agent-archive: backfill: "+action)
		}
		terminal.Println(stderr, "agent-archive: backfill: nothing was imported.")
		return 1
	}
	terminal.Println(stdout, "ready.")
	terminal.Println(stdout)
	backfill.RenderText(stdout, plan)
	terminal.Println(stdout)
	printInterruptedImport(stdout, home, plan, cfg)

	// Step 3: confirm. edit raises the retention of the whole archive and
	// shows the plan again with the new deletion date.
	if !*yes {
		confirmed, err := confirmImport(newPrompter(stdin, stdout), stdout, &plan, cfg.RetentionDays)
		if err != nil {
			terminal.Printf(stderr, "agent-archive: backfill: %v. Nothing was changed.\n", err)
			return 1
		}
		if !confirmed {
			terminal.Println(stdout, "Cancelled. Nothing was changed.")
			return 0
		}
	}
	return importPlan(env, stdout, stderr, home, plan, configFingerprint(cfg), *background)
}

// interruptibleContext returns a context that the first Ctrl-C cancels,
// saying on out that it is stopping. A second Ctrl-C, or SIGTERM or SIGHUP
// at any point, ends the process at once, after removing this process's
// copies of Cursor's database (see watchSignals). stop ends the watch and
// waits for it; it is called once.
func interruptibleContext(env Env, out io.Writer) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	watch := watchSignals(env, out, "agent-archive: backfill: stopping; press Ctrl-C again to quit.", cancel)
	return ctx, func() {
		watch.release()
		cancel()
	}
}

// exitOnSignal ends the process on a signal that stops backfill at once: a
// second Ctrl-C, or SIGTERM or SIGHUP. It first removes the copies of
// Cursor's database this process made, which the Readers holding them would
// otherwise never close: a copy of every chat left in the temporary folder
// until a later sweep. The exit status is the shell's for the signal. A test
// replaces it.
var exitOnSignal = func(sig os.Signal) {
	cursorstore.RemoveOwnSnapshots()
	code := 1
	if s, ok := sig.(syscall.Signal); ok {
		code = 128 + int(s)
	}
	os.Exit(code)
}

// signalWatch watches, while backfill works, for the signals env.interrupts
// delivers: Ctrl-C, SIGTERM, and SIGHUP. The first Ctrl-C calls onFirst and
// says so on out, and the work stops at its next safe point. A second
// Ctrl-C, or a SIGTERM or SIGHUP (sent by a closing terminal or a process
// manager, which will not wait), calls exitOnSignal.
type signalWatch struct {
	stop   func()
	done   chan struct{}
	exited chan struct{}
	seen   atomic.Bool
}

func watchSignals(env Env, out io.Writer, message string, onFirst func()) *signalWatch {
	signals, stop := env.interrupts()
	w := &signalWatch{stop: releaseOnce(stop), done: make(chan struct{}), exited: make(chan struct{})}
	handle := func(sig os.Signal) {
		if sig == os.Interrupt && w.seen.CompareAndSwap(false, true) {
			onFirst()
			terminal.Println(out, message)
			return
		}
		w.seen.Store(true)
		exitOnSignal(sig)
	}
	// A signal already waiting is handled before the work starts.
	select {
	case sig := <-signals:
		handle(sig)
	default:
	}
	go func() {
		defer close(w.exited)
		for {
			select {
			case sig := <-signals:
				handle(sig)
			case <-w.done:
				return
			}
		}
	}()
	return w
}

// requested reports whether a stop was asked for. It never blocks.
func (w *signalWatch) requested() bool { return w.seen.Load() }

// release ends the watch and waits for it, so nothing is written after the
// command returns. It is called once.
func (w *signalWatch) release() {
	close(w.done)
	<-w.exited
	w.stop()
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
// default is No. edit asks for a longer retention period, which it stores
// in plan, and shows the plan again. Retention applies to the whole archive,
// so edit never goes below configured, the retention set now, and does
// nothing while retention is off: either would delete sessions already
// archived (backfill.ApplyToConfig refuses it too).
func confirmImport(p *prompter, out io.Writer, plan *backfill.Plan, configured int) (bool, error) {
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
			if configured <= 0 {
				terminal.Println(out, "Retention is off, so no session is deleted; there is nothing to keep longer.")
				continue
			}
			terminal.Printf(out, "Retention applies to every session in the archive, not only these.\nHere it can only be raised from %d days, and undo puts %d back.\nShorten it in setup.\n", configured, configured)
			days, err := p.retentionDays(plan.RetentionDays)
			if err != nil {
				return false, err
			}
			if days < configured {
				// A shorter retention deletes sessions already archived,
				// hook-captured ones too, which an import never does.
				terminal.Printf(out, "Retention stays at %d days: a shorter period would delete sessions\nalready in the archive. Shorten it in setup.\n", plan.RetentionDays)
				continue
			}
			plan.RetentionDays = days
			terminal.Println(out)
			backfill.RenderText(out, *plan)
			terminal.Println(out)
		default:
			terminal.Println(out, "Please enter y, n, or edit.")
		}
	}
}

// countNoun and isAre are backfill's, so the CLI and the plans it prints
// count alike.
var (
	countNoun = backfill.CountNoun
	isAre     = backfill.IsAre
)

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
// system, and Cursor's database is opened read-only to count the chats only
// it holds.
func (e Env) backfillEnvironment(userHome string) backfill.Environment {
	temps := e.BackfillTempDirs
	if temps == nil {
		temps = append([]string(nil), backfill.DefaultTempDirs...)
		if tmp, ok := e.lookupEnv("TMPDIR"); ok && strings.TrimSpace(tmp) != "" {
			temps = append(temps, strings.TrimSpace(tmp))
		}
	}
	return backfill.Environment{Home: userHome, TempDirs: temps, Now: e.now, CursorDatabase: backfill.CursorDatabaseReader(userHome)}
}

// archiveState answers backfill.ArchiveState from this machine's local store
// and configuration.
type archiveState struct {
	store *state.Store
	cfg   config.Config
}

func newArchiveState(home string, cfg config.Config) archiveState {
	return archiveState{store: state.OpenReadOnly(home), cfg: cfg}
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
	if record.Reason == state.RemovalReasonUndo {
		return backfill.SkipRemovedByUndo, nil
	}
	return backfill.SkipRemovedByRetention, nil
}
