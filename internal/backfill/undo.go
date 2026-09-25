package backfill

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// UndoPlan is what `backfill undo` does to one import. Its sessions are the
// registrations that carry the import's ID, which are the source of truth:
// the batch file's lists can miss sessions a crash registered, and name ones
// that were never registered.
type UndoPlan struct {
	Batch Batch
	// Project is the --project directory, resolved as the import resolved
	// project roots, or "" for the whole import.
	Project string
	// Sessions are removed in this order: subagents before their parents.
	Sessions []UndoSession
	// ExcludeProjects are the projects the import added (or took over, see
	// TakenOver) that are still included and no other import still has
	// sessions in; undo marks them Included: false and keeps their entries.
	ExcludeProjects []archive.ProjectActivation
	// KeepProjects are projects the import added (or took over) that stay
	// included because another import still has sessions there: excluding
	// them would stop those sessions updating without the other import
	// being undone. The batch records them (ProjectsKept) and the imports
	// they are kept for (ProjectsKeptFor); the undo of the last of those
	// with sessions there excludes them.
	KeepProjects []KeptProject
	// RemoveApps are apps the import added to ImportedHarnesses that no
	// imported session left after the undo needs.
	RemoveApps []string
	// TakenOver names, for each project in ExcludeProjects or KeepProjects
	// that the import did not add, the earlier imports whose undo kept it
	// included for this import's sessions. The plan lists them apart.
	TakenOver map[string][]string
	// RestoreRetention is set when the import raised the archive-wide
	// retention and it is still what the import set: undo puts the earlier
	// value back. A --project undo leaves retention alone.
	RestoreRetention *RetentionChange
	// RetentionDeletes counts the sessions, in the current destination and
	// not removed by this undo, that the restored (shorter) retention makes
	// old enough to delete: the next collector pass deletes them.
	RetentionDeletes int
	// RetentionChangedSince is set when the import raised retention and it
	// was changed again afterwards, so undo leaves it as it is.
	RetentionChangedSince bool
	// HookCapturedStopping counts the hook-captured sessions in
	// ExcludeProjects. Excluding the projects stops them uploading; they are
	// not deleted.
	HookCapturedStopping int

	view Plan
}

// KeptProject is a project undo leaves included, and why: the other
// imports that still have sessions there, and how many.
type KeptProject struct {
	Project  archive.ProjectActivation
	Imports  []string
	Sessions int
}

// SharedBatchIDError means sessions registered outside an import carry its
// ID: an earlier import had the same ID and its batch file is gone. Undo
// selects sessions by that ID and cannot tell the two imports apart, so it
// refuses rather than remove both.
type SharedBatchIDError struct {
	Batch    string
	Sessions int
}

func (e *SharedBatchIDError) Error() string {
	return fmt.Sprintf("import %s shares its ID with an earlier import whose file is no longer in imports/: %s carrying the ID %s registered outside import %s's own run. Undo cannot tell the two imports apart, so it removes neither. Nothing was changed",
		e.Batch, CountNoun(e.Sessions, "session"), wasWere(e.Sessions), e.Batch)
}

// UndoSession is one registered session of the import.
type UndoSession struct {
	Registration archive.SessionRegistration
	// InCurrentDestination is set when the session's objects are in the
	// bucket configured now. Otherwise they are in a destination this machine
	// no longer uses, and the session is only forgotten locally.
	InCurrentDestination bool
	// Resumed is set when the app ran the session again after the import
	// (see resumedSinceImport), so it has content newer than the import.
	Resumed bool
	// ResumeUnknown is set for a Cursor database chat whose database could
	// not be read (Cursor held it locked), so whether it was resumed is not
	// known; nothing else showed that it was.
	ResumeUnknown bool
}

// PlanUndo decides what undoing b does. project limits it to one project
// directory, resolved as BuildPlan resolves --project. batches are every
// import, b among them, which decide the projects b's undo takes over from
// earlier undos (see undoProjects). It writes nothing.
//
// It refuses (SharedBatchIDError) when a session carrying b's ID was
// admitted before b started or after it completed: it belongs to an earlier
// import that had the same ID.
func PlanUndo(env Environment, store *state.Store, cfg config.Config, batches []Batch, b Batch, project string) (UndoPlan, error) {
	// Sessions are selected by the ID they carry (InBatch), so a batch
	// without a valid one selects nothing it could prove is its own.
	if !ValidBatchID(b.ID) {
		return UndoPlan{}, fmt.Errorf("import %q has no valid import ID; nothing was changed", b.ID)
	}
	if project != "" {
		if abs, err := filepath.Abs(project); err == nil {
			project = abs
		}
		project = env.resolved(project)
	}
	p := UndoPlan{
		Batch:   b,
		Project: project,
		view: Plan{
			GeneratedAt:   env.now(),
			Home:          env.Home,
			RetentionDays: cfg.RetentionDays,
			resolvedHome:  env.resolved(env.Home),
			Destination: Destination{
				Provider: cfg.Storage.Provider,
				Bucket:   cfg.Storage.Bucket,
				Prefix:   cfg.Storage.Prefix,
			},
		},
	}
	inProject := func(root string) bool {
		return p.Project == "" || root == p.Project || env.resolved(root) == p.Project
	}

	regs, err := store.LoadRegistrations()
	if err != nil {
		return UndoPlan{}, err
	}
	if n := sessionsOutsideBatch(regs, b); n > 0 {
		return UndoPlan{}, &SharedBatchIDError{Batch: b.ID, Sessions: n}
	}
	requests, err := store.LoadRequests()
	if err != nil {
		return UndoPlan{}, err
	}
	requested := map[string]state.Request{}
	for _, req := range requests {
		requested[req.ArchiveSessionID] = req
	}
	// Whether a session's objects are in the bucket configured now. A
	// registration that records its destination ID (always the batch's) is
	// judged by it alone, which stays right after a switch away and back.
	// One without needs both the batch's ID and the time boundary to match.
	sameDestination := b.DestinationID == cfg.DestinationID()
	selected := map[string]bool{}
	var parents, children []UndoSession
	for _, reg := range regs {
		if !InBatch(reg, b.ID) || reg.ParentSessionID != "" || !inProject(reg.ProjectRoot) {
			continue
		}
		selected[reg.ArchiveSessionID] = true
	}
	for _, reg := range regs {
		if !InBatch(reg, b.ID) {
			continue
		}
		// A subagent goes with its parent. One whose parent is no longer
		// registered (an earlier undo removed the parent but failed on the
		// subagent) is selected by its own project, which it shares with the
		// parent.
		if reg.ParentSessionID != "" && !selected[reg.ParentSessionID] && !inProject(reg.ProjectRoot) {
			continue
		}
		if reg.ParentSessionID == "" && !selected[reg.ArchiveSessionID] {
			continue
		}
		resumed, unknown, err := resumedSinceImport(env, store, reg, requested[reg.ArchiveSessionID])
		if err != nil {
			return UndoPlan{}, err
		}
		s := UndoSession{Registration: reg, InCurrentDestination: (reg.DestinationID != "" || sameDestination) && cfg.InCurrentDestination(reg), Resumed: resumed, ResumeUnknown: unknown}
		if reg.ParentSessionID != "" {
			children = append(children, s)
		} else {
			parents = append(parents, s)
		}
	}
	p.Sessions = slices.Concat(children, parents)

	p.ExcludeProjects, p.KeepProjects, p.TakenOver = undoProjects(cfg, regs, batches, b, inProject)
	excludedRoots := map[string]bool{}
	for _, project := range p.ExcludeProjects {
		excludedRoots[project.Root] = true
	}
	// Excluding a project also stops its hook-captured sessions uploading.
	for _, reg := range regs {
		if !reg.Imported() && reg.ParentSessionID == "" && excludedRoots[reg.ProjectRoot] {
			p.HookCapturedStopping++
		}
	}

	if r := b.Retention; r != nil && !r.Restored && p.Project == "" {
		if cfg.RetentionDays == r.To {
			restore := *r
			p.RestoreRetention = &restore
			if p.RetentionDeletes, err = retentionDeletes(store, cfg, regs, p.Sessions, restore, env.now()); err != nil {
				return UndoPlan{}, err
			}
		} else {
			p.RetentionChangedSince = true
		}
	}

	// An app the import added stays while any imported session of it is
	// left: another import's, or this one's outside --project.
	undone := map[string]bool{}
	for _, s := range p.Sessions {
		undone[s.Registration.ArchiveSessionID] = true
	}
	for _, app := range b.AppsAdded {
		if !slices.Contains(cfg.ImportedHarnesses, app) {
			continue
		}
		needed := slices.ContainsFunc(regs, func(reg archive.SessionRegistration) bool {
			return reg.Imported() && !undone[reg.ArchiveSessionID] && canonicalHarness(reg.Harness.Name) == app
		})
		if !needed {
			p.RemoveApps = append(p.RemoveApps, app)
		}
	}
	return p, nil
}

// retentionDeletes counts the sessions (not subagents) in cfg's destination,
// other than those undo removes, that retention of r.From days deletes and
// retention of r.To days does not: those restoring r.From costs. A session
// ages as retention ages it: from its last capture, or its admission when it
// has none (see retention.sweepSession). A capture is never earlier than the
// admission, so only a session admitted over r.From days ago can be one; only
// those have their published state read.
func retentionDeletes(store *state.Store, cfg config.Config, regs []archive.SessionRegistration, removing []UndoSession, r RetentionChange, now time.Time) (int, error) {
	if r.From <= 0 || r.From >= r.To {
		return 0, nil
	}
	removed := map[string]bool{}
	for _, s := range removing {
		removed[s.Registration.ArchiveSessionID] = true
	}
	from, to := time.Duration(r.From)*24*time.Hour, time.Duration(r.To)*24*time.Hour
	n := 0
	for _, reg := range regs {
		if reg.ParentSessionID != "" || removed[reg.ArchiveSessionID] || !cfg.InCurrentDestination(reg) {
			continue
		}
		ageFrom := reg.Admitted()
		if ageFrom.IsZero() || now.Sub(ageFrom) < from {
			continue
		}
		bundle, _, _, found, err := store.LoadPublished(reg.ArchiveSessionID)
		if err != nil {
			return 0, err
		}
		if found && !bundle.Capture.CapturedAt.IsZero() {
			ageFrom = bundle.Capture.CapturedAt
		}
		if age := now.Sub(ageFrom); age >= from && age < to {
			n++
		}
	}
	return n, nil
}

// sessionsOutsideBatch counts the sessions (not subagents) that carry b's ID
// but were admitted before b started or after it completed. Every session
// b's own run registers is admitted at or after StartedAt (the admission is
// stamped when the batch is opened, and a continued batch keeps its start)
// and before CompletedAt, so any other one is an earlier import's that had
// the same ID.
func sessionsOutsideBatch(regs []archive.SessionRegistration, b Batch) int {
	n := 0
	for _, reg := range regs {
		if !InBatch(reg, b.ID) || reg.ParentSessionID != "" || reg.AdmittedAt.IsZero() {
			continue
		}
		if reg.AdmittedAt.Before(b.StartedAt) || b.CompletedAt != nil && reg.AdmittedAt.After(*b.CompletedAt) {
			n++
		}
	}
	return n
}

// undoProjects splits the projects undoing b could exclude into those it
// excludes and those it keeps. The candidates are the included projects in
// scope that b added, and those an earlier undo kept (ProjectsKept) for b
// (ProjectsKeptFor) that b has sessions in: b's undo takes them over, and
// takenOver names, for each, the imports whose undo kept it. A project any
// undo has excluded (ProjectsExcluded of any batch) is never a candidate:
// it is included only because setup included it again.
//
// A candidate stays included while another import has sessions there.
// Excluding it would stop those sessions updating (the configuration no
// longer accepts them), silently, as part of undoing an import they are not
// in. Hook-captured sessions do not keep a project: the import added it, so
// undoing the import is what stops them, as the plan says.
func undoProjects(cfg config.Config, regs []archive.SessionRegistration, batches []Batch, b Batch, inProject func(string) bool) (exclude []archive.ProjectActivation, keep []KeptProject, takenOver map[string][]string) {
	inside := func(reg archive.SessionRegistration, project archive.ProjectActivation) bool {
		return reg.ProjectID == project.ProjectID || reg.ProjectRoot == project.Root
	}
	// A project any undo has excluded is included now only because setup
	// included it again: it is the person's, whichever import added it.
	excludedByUndo := map[string]bool{}
	for _, o := range batches {
		for _, id := range o.ProjectsExcluded {
			excludedByUndo[id] = true
		}
	}
	for _, id := range b.ProjectsExcluded {
		excludedByUndo[id] = true
	}
	takenOver = map[string][]string{}
	for _, project := range cfg.Archive.Projects {
		if !project.Included || !inProject(project.Root) || excludedByUndo[project.ProjectID] {
			continue
		}
		candidate := slices.Contains(b.ProjectsAdded, project.ProjectID)
		if !candidate && slices.ContainsFunc(regs, func(reg archive.SessionRegistration) bool {
			return InBatch(reg, b.ID) && reg.ParentSessionID == "" && inside(reg, project)
		}) {
			// b takes the project over from each earlier undo that kept it
			// for b: one that names b among the imports it kept it for, or,
			// written before those were recorded, names none.
			for _, o := range batches {
				if o.ID == b.ID || o.UndoneAt == nil || !slices.Contains(o.ProjectsKept, project.ProjectID) {
					continue
				}
				if keptFor, recorded := o.ProjectsKeptFor[project.ProjectID]; recorded && !slices.Contains(keptFor, b.ID) {
					continue
				}
				takenOver[project.ProjectID] = append(takenOver[project.ProjectID], o.ID)
			}
			candidate = len(takenOver[project.ProjectID]) > 0
		}
		if !candidate {
			continue
		}
		kept := KeptProject{Project: project}
		for _, reg := range regs {
			if reg.Imported() && reg.ImportBatch != "" && !InBatch(reg, b.ID) && reg.ParentSessionID == "" && inside(reg, project) {
				kept.Sessions++
				kept.Imports = addUnique(kept.Imports, reg.ImportBatch)
			}
		}
		if kept.Sessions == 0 {
			exclude = append(exclude, project)
			continue
		}
		sort.Strings(kept.Imports)
		keep = append(keep, kept)
	}
	return exclude, keep, takenOver
}

// RecordKept records in b the projects its undo keeps included, and the
// imports it keeps each for (ProjectsKept, ProjectsKeptFor), so that only
// those imports' undos take them over.
func (b *Batch) RecordKept(kept []KeptProject) {
	for _, k := range kept {
		id := k.Project.ProjectID
		b.ProjectsKept = addUnique(b.ProjectsKept, id)
		if b.ProjectsKeptFor == nil {
			b.ProjectsKeptFor = map[string][]string{}
		}
		imports := addUnique(append([]string(nil), b.ProjectsKeptFor[id]...), k.Imports...)
		sort.Strings(imports)
		b.ProjectsKeptFor[id] = imports
	}
}

// resumedSinceImport reports whether the app ran an imported session again
// after the import. Two signals, either of which is enough:
//
//   - The transcript was written after the import: its modification time is
//     later than AdmittedAt. Discovery read every transcript before the
//     import was stamped, and nothing in the archive writes to one, so only
//     the app did. This covers apps without hooks, hooks whose payload left
//     no evidence, and content not yet uploaded. For a chat read from
//     Cursor's database, which has no file of its own, it is the chat's
//     lastUpdatedAt, read in place from the database under env.Home.
//   - Hook evidence other than subagent links, waiting in a request, built
//     into a pending publication, or already published. Backfill records
//     none (its links are archive-generated, provenance hook:subagent-link),
//     so any came from a hook. This covers a resume whose transcript moved.
//
// The superseded-source ledger is not a signal: a parent is republished
// when its subagents publish (a link-only change), and a parser upgrade
// republishes everything, neither of which is a resume.
func resumedSinceImport(env Environment, store *state.Store, reg archive.SessionRegistration, req state.Request) (resumed, unknown bool, err error) {
	if reg.SourceKind == archive.SourceKindCursorSQLite && !reg.AdmittedAt.IsZero() {
		sig, err := cursorstore.ReadSignature(context.Background(), CursorStateDatabase(env.Home), reg.SourceKey)
		if err == nil && sig.LastUpdatedAt > reg.AdmittedAt.UnixMilli() {
			return true, false, nil
		}
		// A chat or database that is gone was not resumed; one that could
		// not be read may have been.
		unknown = err != nil && !isNotExist(err)
	}
	resumed, err = resumedByEvidence(env, store, reg, req)
	return resumed, unknown && !resumed, err
}

// resumedByEvidence is resumedSinceImport's transcript time and hook
// evidence signals.
func resumedByEvidence(env Environment, store *state.Store, reg archive.SessionRegistration, req state.Request) (bool, error) {
	if reg.TranscriptPath != "" && !reg.AdmittedAt.IsZero() {
		if info, err := env.stat(reg.TranscriptPath); err == nil && info.ModTime().After(reg.AdmittedAt) {
			return true, nil
		}
	}
	if hasHookEvidence(req.HookEvidence) {
		return true, nil
	}
	// Only the stored bundles' evidence is decoded: decoding the whole
	// pending and published bundles cost several times their size per
	// session, partly under collector.lock.
	evidence, err := store.StoredEvidence(reg.ArchiveSessionID)
	if err != nil {
		return false, err
	}
	return hasHookEvidence(evidence), nil
}

func hasHookEvidence(evidence []archive.SupplementalEvidence) bool {
	return slices.ContainsFunc(evidence, func(e archive.SupplementalEvidence) bool {
		return strings.HasPrefix(e.Provenance, "hook:") && e.Provenance != "hook:subagent-link"
	})
}

// Empty reports whether nothing of the import is left to undo.
func (p UndoPlan) Empty() bool {
	return len(p.Sessions) == 0 && len(p.ExcludeProjects) == 0 && len(p.RemoveApps) == 0 && p.RestoreRetention == nil
}

// UndoCounts summarises an undo's sessions. Sessions and Subagents count
// every one removed; Resumed counts the sessions (not subagents) resumed
// since the import; Deleted and Forgotten split them all by whether the
// bucket is called, and DeletedSessions and ForgottenSessions split the
// sessions alone.
type UndoCounts struct {
	Sessions  int
	Subagents int
	Resumed   int
	// ResumeUnknown counts the Cursor database chats whether resumed could
	// not be checked.
	ResumeUnknown     int
	Deleted           int
	Forgotten         int
	DeletedSessions   int
	ForgottenSessions int
}

// Counts tallies the plan's sessions and subagents into UndoCounts.
func (p UndoPlan) Counts() UndoCounts {
	var c UndoCounts
	for _, s := range p.Sessions {
		parent := s.Registration.ParentSessionID == ""
		if parent {
			c.Sessions++
			if s.Resumed {
				c.Resumed++
			}
			if s.ResumeUnknown {
				c.ResumeUnknown++
			}
		} else {
			c.Subagents++
		}
		if s.InCurrentDestination {
			c.Deleted++
			if parent {
				c.DeletedSessions++
			}
		} else {
			c.Forgotten++
			if parent {
				c.ForgottenSessions++
			}
		}
	}
	return c
}

// ProjectDisplay is the --project directory as the plan shows it, with ~.
func (p UndoPlan) ProjectDisplay() string { return p.view.display(p.Project) }

// Grew reports whether p would remove anything confirmed did not show: a
// session, a bucket delete for a session shown as only forgotten, a project,
// or an app. Undo checks this after confirming, under the locks, and asks
// for a new run rather than removing more than was confirmed. A session
// resumed meanwhile does not count: the person already confirmed deleting
// it, and one in active use would otherwise abort every run.
func (p UndoPlan) Grew(confirmed UndoPlan) bool {
	sessions := map[string]UndoSession{}
	for _, s := range confirmed.Sessions {
		sessions[s.Registration.ArchiveSessionID] = s
	}
	for _, s := range p.Sessions {
		before, ok := sessions[s.Registration.ArchiveSessionID]
		if !ok || s.InCurrentDestination && !before.InCurrentDestination {
			return true
		}
	}
	for _, project := range p.ExcludeProjects {
		if !slices.ContainsFunc(confirmed.ExcludeProjects, func(c archive.ProjectActivation) bool { return c.ProjectID == project.ProjectID }) {
			return true
		}
	}
	for _, app := range p.RemoveApps {
		if !slices.Contains(confirmed.RemoveApps, app) {
			return true
		}
	}
	if p.RestoreRetention != nil && (confirmed.RestoreRetention == nil || p.RetentionDeletes > confirmed.RetentionDeletes) {
		return true
	}
	return false
}

// UndoChanges is what UndoPlan.ApplyToConfig changed; the caller records it
// in the batch once the configuration is saved.
type UndoChanges struct {
	// Excluded are the IDs of the projects excluded.
	Excluded []string
	// RetentionRestored is set when retention was put back to the value
	// before the import.
	RetentionRestored bool
}

// ApplyToConfig excludes the plan's projects, removes its apps, and restores
// the retention from before the import, in cfg, which the caller has just
// reloaded under the locks. Retention is restored only while it is still
// what the import set.
func (p UndoPlan) ApplyToConfig(cfg *config.Config) UndoChanges {
	changes := UndoChanges{Excluded: []string{}}
	for i, project := range cfg.Archive.Projects {
		if project.Included && slices.ContainsFunc(p.ExcludeProjects, func(e archive.ProjectActivation) bool { return e.ProjectID == project.ProjectID }) {
			cfg.Archive.Projects[i].Included = false
			changes.Excluded = append(changes.Excluded, project.ProjectID)
		}
	}
	cfg.ImportedHarnesses = slices.DeleteFunc(cfg.ImportedHarnesses, func(app string) bool { return slices.Contains(p.RemoveApps, app) })
	if r := p.RestoreRetention; r != nil && cfg.RetentionDays == r.To {
		cfg.RetentionDays = r.From
		changes.RetentionRestored = true
	}
	return changes
}

// RecordUndone records in b what an undo changed in the configuration, once
// it is saved, so no later undo changes it again.
func (b *Batch) RecordUndone(c UndoChanges) {
	b.ProjectsExcluded = addUnique(b.ProjectsExcluded, c.Excluded...)
	if c.RetentionRestored && b.Retention != nil {
		b.Retention.Restored = true
	}
}

// UndoResult is what Remove did.
type UndoResult struct {
	// Deleted were removed from the bucket and forgotten; Forgotten, from a
	// previous destination, were only forgotten locally.
	Deleted   []string
	Forgotten []string
	// Failed maps each session left registered to why; running undo again
	// retries it.
	Failed map[string]error
}

// Remove deletes each session of the plan from bucket, as whole-session
// retention does, and forgets it locally with an undo removal record.
// Subagents go before their parents. A failure leaves that session
// registered and moves on to the next. The caller holds collector.lock, so
// no pass publishes a session between its deletion and being forgotten.
// bucket may be nil when no session is in the current destination.
func (p UndoPlan) Remove(ctx context.Context, store *state.Store, bucket storage.ObjectStore, now time.Time) UndoResult {
	result := UndoResult{Failed: map[string]error{}}
	for _, s := range p.Sessions {
		reg := s.Registration
		if s.InCurrentDestination {
			if err := collector.DeleteWholeSession(ctx, bucket, reg.Harness.Name, reg.ArchiveSessionID); err != nil {
				result.Failed[reg.ArchiveSessionID] = fmt.Errorf("delete from the bucket: %w", err)
				continue
			}
		}
		app := canonicalHarness(reg.Harness.Name)
		if app == "" {
			app = reg.Harness.Name
		}
		// deferForWork is off: undo removes the session whatever a hook
		// queued for it meanwhile. ForgetSession also drops the parent's
		// subagent candidates that were never registered.
		if _, err := store.ForgetIdleSession(reg.ArchiveSessionID, reg.NativeSessionID, false, &state.RemovalRecord{
			Harness: app, Reason: state.RemovalReasonUndo, At: now,
		}); err != nil {
			result.Failed[reg.ArchiveSessionID] = fmt.Errorf("forget locally: %w", err)
			continue
		}
		if s.InCurrentDestination {
			result.Deleted = append(result.Deleted, reg.ArchiveSessionID)
		} else {
			result.Forgotten = append(result.Forgotten, reg.ArchiveSessionID)
		}
	}
	return result
}

// RenderUndo prints what the undo will do, before it asks. Like the import
// plan, it names projects and counts only.
func RenderUndo(w io.Writer, p UndoPlan) {
	c := p.Counts()
	scope := "import " + p.Batch.ID
	if p.Project != "" {
		scope += " in " + p.view.display(p.Project)
	}
	terminal.Printf(w, "Undo %s, started %s.\n\n", scope, p.Batch.StartedAt.In(p.view.GeneratedAt.Location()).Format("2006-01-02 15:04"))
	if c.Sessions+c.Subagents > 0 {
		terminal.Println(w, "If you continue:")
	}
	sessions := SessionsAndSubagents(c.Sessions, c.Subagents)
	switch {
	case c.Deleted > 0 && c.Forgotten == 0:
		terminal.Printf(w, "  • %s %s deleted from\n    %s.\n", sessions, IsAre(c.Sessions+c.Subagents), p.view.destination())
	case c.Deleted > 0:
		terminal.Printf(w, "  • %s %s deleted from\n    %s.\n", CountNoun(c.Deleted, "session"), IsAre(c.Deleted), p.view.destination())
		terminal.Printf(w, "  • %s from a previous storage destination %s forgotten on this Mac\n    only; nothing is deleted from that destination.\n", CountNoun(c.Forgotten, "session"), IsAre(c.Forgotten))
	case c.Forgotten > 0:
		terminal.Printf(w, "  • %s from a previous storage destination %s forgotten on this Mac\n    only; nothing is deleted from that destination.\n", sessions, IsAre(c.Sessions+c.Subagents))
	}
	if c.Resumed > 0 {
		terminal.Printf(w, "    This includes %s resumed since the import, with %s newer content.\n", CountNoun(c.Resumed, "session"), theirIts(c.Resumed))
	}
	if n := c.ResumeUnknown; n > 0 {
		terminal.Printf(w, "    Whether %s resumed since the import could not be checked:\n    Cursor's database could not be read.\n", CountNoun(n, "Cursor chat")+" "+wasWere(n))
	}
	if n := len(p.ExcludeProjects); n > 0 {
		if c.Sessions+c.Subagents == 0 {
			terminal.Println(w, "If you continue:")
		}
		// Projects the import added, and those it took over from an earlier
		// undo that kept them for its sessions, are listed apart.
		var added, takenOver []string
		for _, project := range p.ExcludeProjects {
			if from, ok := p.TakenOver[project.ProjectID]; ok {
				takenOver = append(takenOver, fmt.Sprintf("%s (left included by the undo of %s)", p.view.display(project.Root), importsNoun(from)))
			} else {
				added = append(added, p.view.display(project.Root))
			}
		}
		sort.Strings(added)
		sort.Strings(takenOver)
		if k := len(added); k > 0 {
			terminal.Printf(w, "  • %s the import added %s excluded from capture; setup can\n    include %s again:\n", CountNoun(k, "project"), IsAre(k), themIt(k))
			for _, root := range added {
				terminal.Printf(w, "      %s\n", root)
			}
		}
		if k := len(takenOver); k > 0 {
			terminal.Printf(w, "  • %s an earlier undo left included for this import's sessions\n    %s excluded from capture; setup can include %s again:\n", CountNoun(k, "project"), IsAre(k), themIt(k))
			for _, line := range takenOver {
				terminal.Printf(w, "      %s\n", line)
			}
		}
		if h := p.HookCapturedStopping; h > 0 {
			where, verb, what := "these projects", "stop", "they are"
			if n == 1 {
				where = "this project"
			}
			if h == 1 {
				verb, what = "stops", "it is"
			}
			terminal.Printf(w, "    %s in %s %s uploading; %s not deleted.\n", CountNoun(h, "hook-captured session"), where, verb, what)
		}
	}
	if len(p.RemoveApps) > 0 {
		names := make([]string, 0, len(p.RemoveApps))
		for _, app := range p.RemoveApps {
			name := harnessNames[app]
			if name == "" {
				name = app
			}
			names = append(names, name)
		}
		terminal.Printf(w, "  • %s imports without hooks are no longer published.\n", joinAnd(names))
	}
	if n := len(p.KeepProjects); n > 0 {
		if c.Sessions+c.Subagents == 0 && len(p.ExcludeProjects) == 0 {
			terminal.Println(w, "If you continue:")
		}
		which := "the import added"
		if slices.ContainsFunc(p.KeepProjects, func(k KeptProject) bool { _, ok := p.TakenOver[k.Project.ProjectID]; return ok }) {
			which = "the import added or took over"
		}
		terminal.Printf(w, "  • %s %s %s included: other imports still have\n    sessions there, which excluding %s would stop updating:\n", CountNoun(n, "project"), which, stayStays(n), themIt(n))
		kept := make([]string, 0, n)
		for _, k := range p.KeepProjects {
			line := fmt.Sprintf("%s (%s from %s", p.view.display(k.Project.Root), CountNoun(k.Sessions, "session"), importsNoun(k.Imports))
			if from, ok := p.TakenOver[k.Project.ProjectID]; ok {
				line += "; left included by the undo of " + importsNoun(from)
			}
			kept = append(kept, line+")")
		}
		sort.Strings(kept)
		for _, line := range kept {
			terminal.Printf(w, "      %s\n", line)
		}
		terminal.Printf(w, "    Undoing the last of those imports excludes %s.\n", themIt(n))
	}
	if r := p.RestoreRetention; r != nil {
		if c.Sessions+c.Subagents == 0 && len(p.ExcludeProjects) == 0 && len(p.KeepProjects) == 0 {
			terminal.Println(w, "If you continue:")
		}
		terminal.Printf(w, "  • Retention goes back from %d to %d days, as it was before the import\n    raised it. ", r.To, r.From)
		if n := p.RetentionDeletes; n > 0 {
			terminal.Printf(w, "The next collector pass then deletes %s older than\n    %d days from %s.\n", CountNoun(n, "session"), r.From, p.view.destination())
		} else {
			terminal.Println(w, "No session is old enough for that to delete it now.")
		}
	} else if p.RetentionChangedSince {
		terminal.Printf(w, "  • Retention stays at %d days: it was changed after the import raised it.\n", p.view.RetentionDays)
	}
	terminal.Println(w, "  • Hook-captured sessions and the apps' own files are not touched.")
	if c.Sessions+c.Subagents > 0 {
		terminal.Println(w, "  • These sessions are not imported again unless you run\n    agent-archive backfill --include-removed.")
	}
}

// importsNoun names import IDs: "import A", or "imports A and B".
func importsNoun(ids []string) string {
	if len(ids) == 1 {
		return "import " + ids[0]
	}
	return "imports " + joinAnd(ids)
}

// UndoQuestion is the confirmation undo asks.
func UndoQuestion(p UndoPlan) string {
	c := p.Counts()
	if c.Sessions+c.Subagents == 0 {
		if len(p.ExcludeProjects) == 0 && p.RestoreRetention != nil {
			return fmt.Sprintf("Restore retention to %d days?", p.RestoreRetention.From)
		}
		return fmt.Sprintf("Exclude %s?", CountNoun(len(p.ExcludeProjects), "project"))
	}
	// Sessions are named when there are any; otherwise only subagents are
	// left.
	deleted, forgotten := CountNoun(c.DeletedSessions, "session"), CountNoun(c.ForgottenSessions, "session")
	if c.Sessions == 0 {
		deleted, forgotten = CountNoun(c.Deleted, "subagent transcript"), CountNoun(c.Forgotten, "subagent transcript")
	}
	switch {
	case c.Deleted == 0:
		return fmt.Sprintf("Forget %s? This cannot be undone.", forgotten)
	case c.Forgotten == 0:
		return fmt.Sprintf("Delete %s from the archive? This cannot be undone.", deleted)
	}
	return fmt.Sprintf("Delete %s from the archive and forget %s from a previous destination? This cannot be undone.", deleted, forgotten)
}

// SessionsAndSubagents counts sessions and subagent transcripts together,
// naming only the kinds there are: "2 sessions and 1 subagent transcript".
func SessionsAndSubagents(sessions, subagents int) string {
	switch {
	case subagents == 0:
		return CountNoun(sessions, "session")
	case sessions == 0:
		return CountNoun(subagents, "subagent transcript")
	}
	return CountNoun(sessions, "session") + " and " + CountNoun(subagents, "subagent transcript")
}

// IsAre is the verb that agrees with a count of n.
func IsAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func stayStays(n int) string {
	if n == 1 {
		return "stays"
	}
	return "stay"
}

func wasWere(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}

func theirIts(n int) string {
	if n == 1 {
		return "its"
	}
	return "their"
}

func themIt(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}
