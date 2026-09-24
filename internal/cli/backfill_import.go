package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// backfillCheckpoint, when set, is called after the configuration commit
// ("committed") and after each registration hold ("registered"). A test
// returns an error from it to stop the import there, as a crash would.
var backfillCheckpoint func(step string) error

// backfillHoldSteps, when positive, caps the steps registration takes per
// hold of hooks.lock; a test lowers it to force several holds.
var backfillHoldSteps int

func checkpoint(step string) error {
	if backfillCheckpoint == nil {
		return nil
	}
	return backfillCheckpoint(step)
}

// Lock waits. A collector pass can take a while; hooks hold hooks.lock for
// milliseconds.
const (
	backfillCollectorWait = 2 * time.Minute
	backfillHooksWait     = 5 * time.Second
)

// importPlan runs steps 4 to 6 of a confirmed import: it commits the
// configuration, registers the plan's sessions, and uploads them unless
// background is set. setup.lock is held until it returns.
func importPlan(env Env, stdout, stderr io.Writer, home string, plan backfill.Plan, fingerprint string, background bool) int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(stderr, "agent-archive: backfill: "+format+"\n", args...)
		return 1
	}
	releaseSetup, err := local.NamedLock(home, "setup.lock")
	if err != nil {
		return fail("setup or another backfill is running; run backfill again when it finishes. Nothing was changed.")
	}
	defer releaseSetup()

	// Step 4: commit the configuration, under collector.lock and hooks.lock.
	releaseCollector, err := local.NamedLockWait(home, "collector.lock", backfillCollectorWait)
	if err != nil {
		return fail("a collector pass is still running; run backfill again. Nothing was changed.")
	}
	// collector.lock stays held through registration, so no pass runs
	// between a session's subagent candidates and its registration.
	releaseCollector = releaseOnce(releaseCollector)
	defer releaseCollector()
	batch, admittedAt, added, err := commitImport(env, home, plan, fingerprint)
	if err != nil {
		return fail("%v", err)
	}
	if err := checkpoint("committed"); err != nil {
		return fail("%v", err)
	}

	// Step 5: register, in short holds of hooks.lock.
	store, err := collector.NewLocalStore(home)
	if err != nil {
		return fail("open local store: %v", err)
	}
	candidates := plan.Imported()
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].StartedAt.Before(candidates[j].StartedAt) })
	registration := backfill.Registration{
		Home: home, Store: store, Batch: batch.ID, AdmittedAt: admittedAt, MaxHoldSteps: backfillHoldSteps,
		AfterHold: func(sessions, subagents []string) error {
			batch.AddSessions(sessions, subagents)
			if err := backfill.SaveBatch(home, batch); err != nil {
				return err
			}
			return checkpoint("registered")
		},
	}
	result, err := registration.Run(candidates)
	if err != nil {
		if errors.Is(err, backfill.ErrPaused) {
			return fail("%v", err)
		}
		return fail("%v. %s registered before this; run agent-archive backfill again to finish.", err, countNoun(len(result.Sessions), "session"))
	}
	completed := env.now().UTC()
	batch.CompletedAt = &completed
	if err := backfill.SaveBatch(home, batch); err != nil {
		return fail("%v", err)
	}
	releaseCollector()
	printRegistered(stdout, batch.ID, added, result)

	if background {
		fmt.Fprintln(stdout, "The background collector uploads them. Run agent-archive status to follow it.")
		printImportHints(stdout, batch.ID)
		return 0
	}
	// Step 6: upload.
	return uploadImport(env, stdout, stderr, home, batch.ID, plan)
}

// commitImport is step 4. With collector.lock held, it takes hooks.lock,
// rereads the configuration, and checks that it is the one the plan was made
// from. It then stamps the admission time and writes the batch file, then
// the new projects, apps, and retention. The batch file is written first,
// so a crash in between leaves a batch that names projects it did not add,
// never projects added without a record.
func commitImport(env Env, home string, plan backfill.Plan, fingerprint string) (batch backfill.Batch, admittedAt time.Time, added int, err error) {
	releaseHooks, err := local.NamedLockWait(home, "hooks.lock", backfillHooksWait)
	if err != nil {
		return batch, admittedAt, 0, errors.New("capture hooks are busy; run backfill again. Nothing was changed")
	}
	defer releaseHooks()
	cfg, found, err := config.Load(home)
	if err != nil {
		return batch, admittedAt, 0, fmt.Errorf("load config: %w", err)
	}
	if !found {
		return batch, admittedAt, 0, errNotSetUp
	}
	if refusal := importRefusal(home, cfg); refusal != "" {
		return batch, admittedAt, 0, errors.New(refusal)
	}
	if configFingerprint(cfg) != fingerprint {
		return batch, admittedAt, 0, errors.New("the configuration changed while this was open; run backfill again. Nothing was changed")
	}
	now := env.now()
	admittedAt = now.UTC()
	if err := backfill.CheckClock(cfg, plan, admittedAt); err != nil {
		return batch, admittedAt, 0, fmt.Errorf("%w. Nothing was changed", err)
	}
	batch, err = backfill.OpenBatch(home, backfill.NewBatchFilters(plan.Filters), backfill.DestinationID(cfg.Storage), now)
	if err != nil {
		return batch, admittedAt, 0, err
	}
	projects, apps := backfill.ApplyToConfig(&cfg, plan, admittedAt)
	if plan.RetentionDays > 0 {
		cfg.RetentionDays = plan.RetentionDays
	}
	batch.AddChanges(projects, apps)
	if err := backfill.SaveBatch(home, batch); err != nil {
		return batch, admittedAt, 0, err
	}
	if err := config.Save(home, cfg); err != nil {
		return batch, admittedAt, 0, fmt.Errorf("save config: %w", err)
	}
	return batch, admittedAt, len(projects), nil
}

func printRegistered(out io.Writer, batchID string, added int, result backfill.RegistrationResult) {
	line := ""
	if added > 0 {
		line = fmt.Sprintf("Added %s. ", countNoun(added, "project"))
	}
	line += "Registered " + countNoun(len(result.Sessions), "session")
	if n := len(result.Subagents); n > 0 {
		line += " and " + countNoun(n, "subagent transcript")
	}
	fmt.Fprintf(out, "%s as import %s.\n", line, batchID)
	var skipped []string
	if n := result.AlreadyArchived; n > 0 {
		skipped = append(skipped, fmt.Sprintf("%d already in the archive", n))
	}
	if n := result.Gone; n > 0 {
		skipped = append(skipped, fmt.Sprintf("%d whose transcript is gone", n))
	}
	if n := result.NotAdmitted; n > 0 {
		skipped = append(skipped, fmt.Sprintf("%d no longer accepted by the setup", n))
	}
	if len(skipped) > 0 {
		fmt.Fprintf(out, "Changed since the plan, and not registered: %s.\n", strings.Join(skipped, ", "))
	}
}

func printImportHints(out io.Writer, batchID string) {
	fmt.Fprintln(out, "See them with `agent-archive list --imported`.")
	fmt.Fprintf(out, "Undo with `agent-archive backfill undo %s`.\n", batchID)
}

// uploadBusyGiveUp is how long upload waits on a collector pass that holds
// the lock without finishing any of the import's sessions.
const uploadBusyGiveUp = 2 * time.Minute

// uploadImport is step 6: collector passes, as sync runs them, until no
// session of the batch has work left or a pass makes no progress. Ctrl-C
// ends the pass after the session in flight; what is left is uploaded by
// the background collector.
func uploadImport(env Env, stdout, stderr io.Writer, home, batchID string, plan backfill.Plan) int {
	sizes := map[string]int64{}
	for _, c := range plan.Candidates {
		size := c.Bytes
		for _, sub := range c.Subagents {
			size += sub.Bytes
		}
		sizes[c.Harness+"\x00"+c.NativeSessionID] = size
	}
	u := &upload{env: env, home: home, batch: batchID, sizes: sizes, terminal: env.isTerminal(stdout), out: stdout}
	if err := u.refresh(); err != nil {
		fmt.Fprintf(stderr, "agent-archive: backfill: %v\n", err)
		return 1
	}
	signals, stopSignals := env.interrupts()
	defer stopSignals()
	// Ctrl-C is looked for before each session and between passes, so the
	// session in flight always finishes.
	interrupted := false
	stop := func() bool {
		select {
		case <-signals:
			interrupted = true
		default:
		}
		return interrupted
	}

	var passErr error
	lastProgress := time.Now()
	for len(u.pending) > 0 && !stop() {
		before := len(u.pending)
		_, err := runPass(env, false, passOptions{progress: u.observe, stop: stop})
		if refreshErr := u.refresh(); refreshErr != nil && err == nil {
			err = refreshErr
		}
		if len(u.pending) < before {
			lastProgress = time.Now()
		}
		if errors.Is(err, local.ErrBusy) {
			// The background collector is running a pass, which uploads
			// these sessions too. Wait for it.
			if time.Since(lastProgress) > uploadBusyGiveUp {
				break
			}
			select {
			case <-time.After(2 * time.Second):
			case <-signals:
				interrupted = true
			}
			continue
		}
		if err != nil {
			passErr = err
			break
		}
		u.draw(!u.terminal)
		if len(u.pending) >= before {
			break
		}
	}
	if u.terminal && u.drawn {
		fmt.Fprintln(stdout)
	}

	switch {
	case len(u.pending) == 0:
		fmt.Fprintf(stdout, "Uploaded %s (%s).\n", countNoun(u.total, "session"), backfill.FormatSize(u.totalBytes))
	case interrupted:
		fmt.Fprintf(stdout, "Stopped. The remaining %s will be uploaded by the background collector.\n", countNoun(len(u.pending), "session"))
	case errors.Is(passErr, errPaused):
		fmt.Fprintf(stdout, "Collection is paused. The remaining %s will be uploaded after agent-archive resume.\n", countNoun(len(u.pending), "session"))
	default:
		fmt.Fprintf(stdout, "%s not uploaded yet. The background collector keeps trying; run agent-archive status to follow it.\n", countNoun(len(u.pending), "session"))
	}
	printImportHints(stdout, batchID)
	if interrupted || errors.Is(passErr, errPaused) {
		return 0
	}
	if passErr != nil {
		fmt.Fprintf(stderr, "agent-archive: backfill: upload: %v\n", passErr)
		return 1
	}
	if len(u.pending) > 0 {
		return 1
	}
	return 0
}

// upload tracks a batch's sessions while they upload.
type upload struct {
	env      Env
	home     string
	batch    string
	sizes    map[string]int64
	terminal bool
	out      io.Writer
	drawn    bool

	total      int
	totalBytes int64
	// pending maps each session still to upload to its size.
	pending map[string]int64
}

// refresh recounts the batch's sessions from the local store: registrations
// in the batch, not subagents, that still have upload work.
func (u *upload) refresh() error {
	cfg, found, err := config.Load(u.home)
	if err != nil {
		return err
	}
	if !found {
		return errNotSetUp
	}
	store := collector.OpenLocalStoreReadOnly(u.home)
	regs, err := store.LoadRegistrations()
	if err != nil {
		return err
	}
	u.total, u.totalBytes, u.pending = 0, 0, map[string]int64{}
	for _, reg := range regs {
		if reg.ImportBatch != u.batch || reg.ParentSessionID != "" {
			continue
		}
		size := u.size(reg)
		u.total++
		u.totalBytes += size
		waiting, err := importPending(store, cfg, reg)
		if err != nil {
			return err
		}
		if waiting {
			u.pending[reg.ArchiveSessionID] = size
		}
	}
	return nil
}

func (u *upload) size(reg archive.SessionRegistration) int64 {
	if size, ok := u.sizes[reg.Harness.Name+"\x00"+reg.NativeSessionID]; ok {
		return size
	}
	if info, err := os.Stat(reg.TranscriptPath); err == nil {
		return info.Size()
	}
	return 0
}

// observe is the collector's progress callback: a published session of the
// batch is done.
func (u *upload) observe(p collector.Progress) {
	if _, ok := u.pending[p.ArchiveSessionID]; !ok || !p.Published {
		return
	}
	delete(u.pending, p.ArchiveSessionID)
	if u.terminal {
		u.draw(false)
	}
}

// draw shows the progress line: redrawn in place on a terminal, or printed
// once per pass otherwise.
func (u *upload) draw(newline bool) {
	var pendingBytes int64
	for _, size := range u.pending {
		pendingBytes += size
	}
	line := fmt.Sprintf("Uploading: %d of %s, %s of %s", u.total-len(u.pending), countNoun(u.total, "session"), backfill.FormatSize(u.totalBytes-pendingBytes), backfill.FormatSize(u.totalBytes))
	if newline {
		fmt.Fprintln(u.out, line)
		return
	}
	fmt.Fprintf(u.out, "\r%-72s", line)
	u.drawn = true
}

// runBackfillHistory implements `agent-archive backfill history`: each
// import with its ID, start, sessions, projects added, and upload state. It
// reads local state only.
func runBackfillHistory(args []string, stdout, stderr io.Writer, env Env) int {
	if len(args) != 0 {
		fmt.Fprintf(stderr, "agent-archive: backfill history: unexpected argument %q\n", args[0])
		return 2
	}
	home, err := env.readHome()
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: backfill history: resolve home: %v\n", err)
		return 1
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: backfill history: load config: %v\n", err)
		return 1
	}
	if !found {
		fmt.Fprintln(stdout, notSetUpMessage)
		return 0
	}
	batches, err := backfill.LoadBatches(home)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: backfill history: %v\n", err)
		return 1
	}
	if len(batches) == 0 {
		fmt.Fprintln(stdout, "No imports yet. Run agent-archive backfill --dry-run to see what an import would do.")
		return 0
	}
	store := collector.OpenLocalStoreReadOnly(home)
	regs, err := store.LoadRegistrations()
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: backfill history: %v\n", err)
		return 1
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "IMPORT\tSTARTED\tSESSIONS\tPROJECTS ADDED\tUPLOAD")
	loc := env.now().Location()
	for _, b := range batches {
		state, err := batchUploadState(store, cfg, regs, b)
		if err != nil {
			fmt.Fprintf(stderr, "agent-archive: backfill history: %v\n", err)
			return 1
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\n", b.ID, b.StartedAt.In(loc).Format("2006-01-02 15:04"), len(b.Sessions), len(b.ProjectsAdded), state)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "agent-archive: backfill history: %v\n", err)
		return 1
	}
	return 0
}

// batchUploadState is history's UPLOAD column: interrupted, removed (none
// of its sessions is registered any more), how many are waiting, or
// uploaded.
func batchUploadState(store *collector.LocalStore, cfg config.Config, regs []archive.SessionRegistration, b backfill.Batch) (string, error) {
	if b.CompletedAt == nil {
		return "interrupted; run agent-archive backfill to finish", nil
	}
	registered, waiting := 0, 0
	for _, reg := range regs {
		if reg.ImportBatch != b.ID || reg.ParentSessionID != "" {
			continue
		}
		registered++
		pending, err := importPending(store, cfg, reg)
		if err != nil {
			return "", err
		}
		if pending {
			waiting++
		}
	}
	switch {
	case registered == 0 && len(b.Sessions) > 0:
		return "removed", nil
	case waiting > 0:
		return fmt.Sprintf("%d waiting", waiting), nil
	}
	return "uploaded", nil
}
