package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// backfillCheckpoint, when set, is called after the configuration commit
// ("committed"), after each registration hold ("registered"), before the
// upload ("uploading"), and when undo holds its locks and has rechecked its
// plan ("undoing"). A test
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

	// Ctrl-C from here on stops between registration holds, or before the
	// next session uploads. After the first, a second one quits at once.
	stdout = &lockedWriter{w: stdout}
	interrupt := newInterruption(env, stdout)
	defer interrupt.release()

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
		Stop: interrupt.requested,
	}
	result, err := registration.Run(candidates)
	if err != nil {
		// Whatever the last hold registered is in the store even if the
		// batch file missed it; record it before stopping.
		if reconcileErr := batch.Reconcile(store); reconcileErr == nil {
			_ = backfill.SaveBatch(home, batch)
		}
		if errors.Is(err, backfill.ErrStopped) {
			fmt.Fprintf(stdout, "Stopped. %s registered as import %s; run agent-archive backfill again with the same options to finish it.\n", countNoun(len(batch.Sessions), "session"), batch.ID)
			return 1
		}
		return fail("%v. %s registered before this; run agent-archive backfill again with the same options to finish.", err, countNoun(len(result.Sessions), "session"))
	}
	if err := completeBatch(env, home, store, &batch); err != nil {
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
	if err := checkpoint("uploading"); err != nil {
		return fail("%v", err)
	}
	return uploadImport(env, stdout, stderr, home, batch.ID, plan, interrupt)
}

// completeBatch rebuilds the batch's sessions from the registrations, which
// are the source of truth, and marks it complete.
func completeBatch(env Env, home string, store *collector.LocalStore, batch *backfill.Batch) error {
	if err := batch.Reconcile(store); err != nil {
		return err
	}
	completed := env.now().UTC()
	batch.CompletedAt = &completed
	return backfill.SaveBatch(home, *batch)
}

// finishInterruptedBatch completes the latest import when it was
// interrupted, a run with the same options finds nothing left to import,
// and so no later run would ever complete it.
func finishInterruptedBatch(env Env, stdout io.Writer, home string, plan backfill.Plan, cfg config.Config) error {
	batches, err := backfill.LoadBatches(home)
	if err != nil || len(batches) == 0 {
		return err
	}
	last := batches[len(batches)-1]
	if !last.Continues(plan.BatchFilters(), backfill.DestinationID(cfg.Storage)) {
		return nil
	}
	release, err := local.NamedLock(home, "setup.lock")
	if err != nil {
		return errors.New("setup or another backfill is running; run backfill again when it finishes")
	}
	defer release()
	// No collector pass or retention may remove what is being listed.
	releaseCollector, err := local.NamedLockWait(home, "collector.lock", backfillCollectorWait)
	if err != nil {
		return errors.New("a collector pass is still running; run backfill again")
	}
	defer releaseCollector()
	// Read the batch again under the locks: an undo or another run may have
	// changed it since it was checked.
	if batches, err = backfill.LoadBatches(home); err != nil || len(batches) == 0 {
		return err
	}
	latest := batches[len(batches)-1]
	if latest.ID != last.ID || !latest.Continues(plan.BatchFilters(), backfill.DestinationID(cfg.Storage)) {
		return nil
	}
	last = latest
	if err := completeBatch(env, home, collector.OpenLocalStoreReadOnly(home), &last); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\nImport %s, which was interrupted, is complete: %s registered.\n", last.ID, countNoun(len(last.Sessions), "session"))
	return nil
}

// interruption watches for Ctrl-C. The first one is recorded and stops the
// watch at once, even in the middle of a long upload, so a second one ends
// the process as usual.
type interruption struct {
	stop         func()
	done, exited chan struct{}
	seen         atomic.Bool
}

func newInterruption(env Env, out io.Writer) *interruption {
	signals, stop := env.interrupts()
	i := &interruption{stop: releaseOnce(stop), done: make(chan struct{}), exited: make(chan struct{})}
	go func() {
		defer close(i.exited)
		select {
		case <-signals:
			i.seen.Store(true)
			i.stop()
			fmt.Fprintln(out, "Stopping after the current session; press Ctrl-C again to quit.")
		case <-i.done:
		}
	}()
	return i
}

// requested reports whether Ctrl-C has been pressed. It never blocks.
func (i *interruption) requested() bool { return i.seen.Load() }

// release ends the watch and waits for it, so nothing is written after
// the command returns. It is called once.
func (i *interruption) release() {
	close(i.done)
	<-i.exited
	i.stop()
}

// lockedWriter serializes writes, so the interruption's message never
// interleaves with the command's own output.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// underlyingWriter is w without a lockedWriter around it, for terminal
// checks.
func underlyingWriter(w io.Writer) io.Writer {
	if l, ok := w.(*lockedWriter); ok {
		return l.w
	}
	return w
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
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
	batch, err = backfill.OpenBatch(home, plan.BatchFilters(), backfill.DestinationID(cfg.Storage), now)
	if err != nil {
		return batch, admittedAt, 0, fmt.Errorf("%w. Nothing was changed. Repair or move the unreadable file out of %s, then run backfill again", err, filepath.Join(home, "imports"))
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
	if n := result.Invalid; n > 0 {
		skipped = append(skipped, fmt.Sprintf("%d that could not be registered", n))
	}
	if n := result.SubagentsInvalid; n > 0 {
		skipped = append(skipped, fmt.Sprintf("%s whose record conflicts with an earlier one or is incomplete", countNoun(n, "subagent transcript")))
	}
	if len(skipped) > 0 {
		fmt.Fprintf(out, "Not registered: %s.\n", strings.Join(skipped, ", "))
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
func uploadImport(env Env, stdout, stderr io.Writer, home, batchID string, plan backfill.Plan, interrupt *interruption) int {
	sizes := map[string]int64{}
	for _, c := range plan.Candidates {
		size := c.Bytes
		for _, sub := range c.Subagents {
			size += sub.Bytes
		}
		sizes[c.Harness+"\x00"+c.NativeSessionID] = size
	}
	u := &upload{env: env, home: home, batch: batchID, sizes: sizes, terminal: env.isTerminal(underlyingWriter(stdout)), out: stdout}
	if err := u.refresh(); err != nil {
		fmt.Fprintf(stderr, "agent-archive: backfill: %v\n", err)
		return 1
	}
	// Ctrl-C is looked for before each session and between passes, so the
	// session in flight always finishes.
	stop := interrupt.requested

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
			for wait := 0; wait < 20 && !stop(); wait++ {
				time.Sleep(100 * time.Millisecond)
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
	case interrupt.requested():
		fmt.Fprintf(stdout, "Stopped. The remaining %s will be uploaded by the background collector.\n", countNoun(len(u.pending), "session"))
	case errors.Is(passErr, errPaused):
		fmt.Fprintf(stdout, "Collection is paused. The remaining %s will be uploaded after agent-archive resume.\n", countNoun(len(u.pending), "session"))
	default:
		fmt.Fprintf(stdout, "%s not uploaded yet. The background collector keeps trying; run agent-archive status to follow it.\n", countNoun(len(u.pending), "session"))
	}
	printImportHints(stdout, batchID)
	if interrupt.requested() || errors.Is(passErr, errPaused) {
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
		// An unreadable import file is named; the others are still listed.
		fmt.Fprintf(stderr, "agent-archive: backfill history: %v\n", err)
	}
	if len(batches) == 0 && err != nil {
		fmt.Fprintln(stdout, "No import could be read. Repair or move the files named above, then run agent-archive backfill history again.")
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
	for i, b := range batches {
		state, err := batchUploadState(store, cfg, regs, b, i == len(batches)-1)
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

// batchUploadState is history's UPLOAD column: undone, or partly undone
// with what is left; interrupted, nothing registered, removed (none of its
// sessions is registered any more), how many are waiting, or uploaded. Only
// the latest import can still be finished by running backfill again.
func batchUploadState(store *collector.LocalStore, cfg config.Config, regs []archive.SessionRegistration, b backfill.Batch, latest bool) (string, error) {
	registered, subagents, waiting := 0, 0, 0
	for _, reg := range regs {
		if reg.ImportBatch != b.ID {
			continue
		}
		if reg.ParentSessionID != "" {
			subagents++
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
	if b.UndoneAt != nil {
		projects := 0
		for _, p := range cfg.Archive.Projects {
			if p.Included && slices.Contains(b.ProjectsAdded, p.ProjectID) && !slices.Contains(b.ProjectsExcluded, p.ProjectID) {
				projects++
			}
		}
		switch {
		case registered > 0:
			return fmt.Sprintf("partly undone; %s left", countNoun(registered, "session")), nil
		case subagents > 0:
			return fmt.Sprintf("partly undone; %s left", countNoun(subagents, "subagent transcript")), nil
		case projects > 0:
			return fmt.Sprintf("partly undone; %s left", countNoun(projects, "project")), nil
		}
		return "undone", nil
	}
	switch {
	case b.CompletedAt == nil && latest:
		return fmt.Sprintf("interrupted; %d registered; run agent-archive backfill with the same options to finish", registered), nil
	case b.CompletedAt == nil:
		return fmt.Sprintf("interrupted; %d registered", registered), nil
	case len(b.Sessions) == 0:
		return "nothing registered", nil
	case registered == 0:
		return "removed", nil
	case waiting > 0:
		return fmt.Sprintf("%d waiting", waiting), nil
	}
	return "uploaded", nil
}
