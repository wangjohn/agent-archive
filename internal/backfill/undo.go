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
	"github.com/wangjohn/agent-archive/internal/storage"
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
	// ExcludeProjects are the projects the import added that are still
	// included; undo marks them Included: false and keeps their entries.
	ExcludeProjects []archive.ProjectActivation
	// RemoveApps are apps the import added to ImportedHarnesses that no
	// imported session left after the undo needs.
	RemoveApps []string
	// HookCapturedStopping counts the hook-captured sessions in
	// ExcludeProjects. Excluding the projects stops them uploading; they are
	// not deleted.
	HookCapturedStopping int

	view Plan
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
}

// PlanUndo decides what undoing b does. project limits it to one project
// directory, resolved as BuildPlan resolves --project. It writes nothing.
func PlanUndo(env Environment, store *collector.LocalStore, cfg config.Config, b Batch, project string) (UndoPlan, error) {
	p := UndoPlan{Batch: b, view: Plan{GeneratedAt: env.now(),
		Home: env.Home, resolvedHome: env.resolved(env.Home),
		Destination: Destination{Provider: cfg.Storage.Provider, Bucket: cfg.Storage.Bucket, Prefix: cfg.Storage.Prefix},
	}}
	if project != "" {
		if abs, err := filepath.Abs(project); err == nil {
			project = abs
		}
		p.Project = env.resolved(project)
	}
	inProject := func(root string) bool {
		return p.Project == "" || root == p.Project || env.resolved(root) == p.Project
	}

	regs, err := store.LoadRegistrations()
	if err != nil {
		return UndoPlan{}, err
	}
	requests, err := store.LoadRequests()
	if err != nil {
		return UndoPlan{}, err
	}
	requested := map[string]collector.Request{}
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
		if reg.ImportBatch != b.ID || reg.ParentSessionID != "" || !inProject(reg.ProjectRoot) {
			continue
		}
		selected[reg.ArchiveSessionID] = true
	}
	for _, reg := range regs {
		if reg.ImportBatch != b.ID {
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
		resumed, err := resumedSinceImport(env, store, reg, requested[reg.ArchiveSessionID])
		if err != nil {
			return UndoPlan{}, err
		}
		s := UndoSession{Registration: reg, InCurrentDestination: (reg.DestinationID != "" || sameDestination) && cfg.InCurrentDestination(reg), Resumed: resumed}
		if reg.ParentSessionID != "" {
			children = append(children, s)
		} else {
			parents = append(parents, s)
		}
	}
	p.Sessions = append(children, parents...)

	excludedRoots := map[string]bool{}
	for _, project := range cfg.Archive.Projects {
		if project.Included && slices.Contains(b.ProjectsAdded, project.ProjectID) && !slices.Contains(b.ProjectsExcluded, project.ProjectID) && inProject(project.Root) {
			p.ExcludeProjects = append(p.ExcludeProjects, project)
			excludedRoots[project.Root] = true
		}
	}
	// Excluding a project also stops its hook-captured sessions uploading.
	for _, reg := range regs {
		if !reg.Imported() && reg.ParentSessionID == "" && excludedRoots[reg.ProjectRoot] {
			p.HookCapturedStopping++
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
func resumedSinceImport(env Environment, store *collector.LocalStore, reg archive.SessionRegistration, req collector.Request) (bool, error) {
	if reg.SourceKind == archive.SourceKindCursorSQLite && !reg.AdmittedAt.IsZero() {
		sig, err := cursorstore.ReadSignature(context.Background(), CursorStateDatabase(env.Home), reg.SourceKey)
		if err == nil && sig.LastUpdatedAt > reg.AdmittedAt.UnixMilli() {
			return true, nil
		}
	}
	if reg.TranscriptPath != "" && !reg.AdmittedAt.IsZero() {
		if info, err := env.stat(reg.TranscriptPath); err == nil && info.ModTime().After(reg.AdmittedAt) {
			return true, nil
		}
	}
	if hasHookEvidence(req.HookEvidence) {
		return true, nil
	}
	pending, found, err := store.LoadPending(reg.ArchiveSessionID)
	if err != nil {
		return false, err
	}
	if found && hasHookEvidence(pending.Bundle.SupplementalEvidence) {
		return true, nil
	}
	bundle, _, _, found, err := store.LoadPublished(reg.ArchiveSessionID)
	if err != nil {
		return false, err
	}
	return found && hasHookEvidence(bundle.SupplementalEvidence), nil
}

func hasHookEvidence(evidence []archive.SupplementalEvidence) bool {
	return slices.ContainsFunc(evidence, func(e archive.SupplementalEvidence) bool {
		return strings.HasPrefix(e.Provenance, "hook:") && e.Provenance != "hook:subagent-link"
	})
}

// Empty reports whether nothing of the import is left to undo.
func (p UndoPlan) Empty() bool {
	return len(p.Sessions) == 0 && len(p.ExcludeProjects) == 0 && len(p.RemoveApps) == 0
}

// UndoCounts summarises an undo's sessions. Sessions and Subagents count
// every one removed; Resumed counts the sessions (not subagents) resumed
// since the import; Deleted and Forgotten split them all by whether the
// bucket is called, and DeletedSessions and ForgottenSessions split the
// sessions alone.
type UndoCounts struct {
	Sessions, Subagents, Resumed       int
	Deleted, Forgotten                 int
	DeletedSessions, ForgottenSessions int
}

func (p UndoPlan) Counts() UndoCounts {
	var c UndoCounts
	for _, s := range p.Sessions {
		parent := s.Registration.ParentSessionID == ""
		if parent {
			c.Sessions++
			if s.Resumed {
				c.Resumed++
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
	return false
}

// ApplyToConfig excludes the plan's projects and removes its apps from cfg,
// which the caller has just reloaded under the locks. It returns the IDs of
// the projects it excluded.
func (p UndoPlan) ApplyToConfig(cfg *config.Config) []string {
	excluded := []string{}
	for i, project := range cfg.Archive.Projects {
		if project.Included && slices.ContainsFunc(p.ExcludeProjects, func(e archive.ProjectActivation) bool { return e.ProjectID == project.ProjectID }) {
			cfg.Archive.Projects[i].Included = false
			excluded = append(excluded, project.ProjectID)
		}
	}
	cfg.ImportedHarnesses = slices.DeleteFunc(cfg.ImportedHarnesses, func(app string) bool { return slices.Contains(p.RemoveApps, app) })
	return excluded
}

// UndoResult is what Remove did.
type UndoResult struct {
	// Deleted were removed from the bucket and forgotten; Forgotten, from a
	// previous destination, were only forgotten locally.
	Deleted, Forgotten []string
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
func (p UndoPlan) Remove(ctx context.Context, store *collector.LocalStore, bucket storage.ObjectStore, now time.Time) UndoResult {
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
		if _, err := store.ForgetIdleSession(reg.ArchiveSessionID, reg.NativeSessionID, false, &collector.RemovalRecord{
			Harness: app, Reason: collector.RemovalReasonUndo, At: now,
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
	fmt.Fprintf(w, "Undo %s, started %s.\n\n", scope, p.Batch.StartedAt.In(p.view.GeneratedAt.Location()).Format("2006-01-02 15:04"))
	if c.Sessions+c.Subagents > 0 {
		fmt.Fprintln(w, "If you continue:")
	}
	sessions := undoNoun(c.Sessions, c.Subagents)
	switch {
	case c.Deleted > 0 && c.Forgotten == 0:
		fmt.Fprintf(w, "  • %s %s deleted from\n    %s.\n", sessions, isAre(c.Sessions+c.Subagents), p.view.destination())
	case c.Deleted > 0:
		fmt.Fprintf(w, "  • %s %s deleted from\n    %s.\n", count(c.Deleted, "session"), isAre(c.Deleted), p.view.destination())
		fmt.Fprintf(w, "  • %s from a previous storage destination %s forgotten on this Mac\n    only; nothing is deleted from that destination.\n", count(c.Forgotten, "session"), isAre(c.Forgotten))
	case c.Forgotten > 0:
		fmt.Fprintf(w, "  • %s from a previous storage destination %s forgotten on this Mac\n    only; nothing is deleted from that destination.\n", sessions, isAre(c.Sessions+c.Subagents))
	}
	if c.Resumed > 0 {
		fmt.Fprintf(w, "    This includes %s resumed since the import, with %s newer content.\n", count(c.Resumed, "session"), theirIts(c.Resumed))
	}
	if n := len(p.ExcludeProjects); n > 0 {
		if c.Sessions+c.Subagents == 0 {
			fmt.Fprintln(w, "If you continue:")
		}
		fmt.Fprintf(w, "  • %s the import added %s excluded from capture; setup can\n    include %s again:\n", count(n, "project"), isAre(n), themIt(n))
		roots := make([]string, 0, n)
		for _, project := range p.ExcludeProjects {
			roots = append(roots, p.view.display(project.Root))
		}
		sort.Strings(roots)
		for _, root := range roots {
			fmt.Fprintf(w, "      %s\n", root)
		}
		if h := p.HookCapturedStopping; h > 0 {
			where, verb, what := "these projects", "stop", "they are"
			if n == 1 {
				where = "this project"
			}
			if h == 1 {
				verb, what = "stops", "it is"
			}
			fmt.Fprintf(w, "    %s in %s %s uploading; %s not deleted.\n", count(h, "hook-captured session"), where, verb, what)
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
		fmt.Fprintf(w, "  • %s imports without hooks are no longer published.\n", joinAnd(names))
	}
	fmt.Fprintln(w, "  • Hook-captured sessions and the apps' own files are not touched.")
	if c.Sessions+c.Subagents > 0 {
		fmt.Fprintln(w, "  • These sessions are not imported again unless you run\n    agent-archive backfill --include-removed.")
	}
}

// UndoQuestion is the confirmation undo asks.
func UndoQuestion(p UndoPlan) string {
	c := p.Counts()
	if c.Sessions+c.Subagents == 0 {
		return fmt.Sprintf("Exclude %s?", count(len(p.ExcludeProjects), "project"))
	}
	// Sessions are named when there are any; otherwise only subagents are
	// left.
	deleted, forgotten := count(c.DeletedSessions, "session"), count(c.ForgottenSessions, "session")
	if c.Sessions == 0 {
		deleted, forgotten = count(c.Deleted, "subagent transcript"), count(c.Forgotten, "subagent transcript")
	}
	switch {
	case c.Deleted == 0:
		return fmt.Sprintf("Forget %s? This cannot be undone.", forgotten)
	case c.Forgotten == 0:
		return fmt.Sprintf("Delete %s from the archive? This cannot be undone.", deleted)
	}
	return fmt.Sprintf("Delete %s from the archive and forget %s from a previous destination? This cannot be undone.", deleted, forgotten)
}

func undoNoun(sessions, subagents int) string {
	switch {
	case subagents == 0:
		return count(sessions, "session")
	case sessions == 0:
		return count(subagents, "subagent transcript")
	}
	return count(sessions, "session") + " and " + count(subagents, "subagent transcript")
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
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
