package backfill

import (
	"context"
	"errors"
	"path/filepath"
	"slices"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

// prepareRecoveryInventory separates observed repository membership from
// committed capture policy. Discovery has finished; output filters have not run.
// Gaps stay per app: each session's recorded recovery is blocked only by the
// gaps that can affect it (see recoveryGaps.blocking).
func prepareRecoveryInventory(ctx context.Context, r *resolver, items []*work, unread unreadable, dbGaps recoveryGaps) {
	items = recoveryEvidenceItems(items)
	projects, witnesses, gaps := recoveryWitnessInventory(ctx, r, items)
	gaps.merge(dbGaps)
	gaps.addUnreadable(unread)
	r.recoveryGaps = gaps
	selectedRoots := map[string]bool{}
	r.requireWitnessFormats = func(root string) {
		selectedRoots[root] = true
		for _, w := range witnesses[root] {
			w.proposedWitness = true
		}
	}
	r.proposedRootEligible = func(root string) bool {
		for _, w := range witnesses[root] {
			if w.validated && w.importable() && !w.vanished && !w.evidenceOnly {
				return true
			}
		}
		return false
	}
	ownershipCurrent := recoveryOwnershipCurrent(r, items)
	validation := &recoveryWitnessValidation{ctx: ctx}
	r.recoverySourcesReset = func(ctx context.Context) {
		validation.ctx = ctx
		validation.checked, validation.current = false, false
		if r.workspaceReset != nil {
			r.workspaceReset()
		}
	}
	r.recoverySourcesCurrent = func() bool {
		if !validation.checked {
			validation.checked = true
			validation.current = recoveryWitnessesCurrent(validation.ctx, r, witnesses, selectedRoots) && ownershipCurrent(validation.ctx)
		}
		return validation.current
	}
	r.mappingRecovery = r.recovery
	r.recovery = sourcefacts.NewRecoveryResolver(projects, r.filters.ProjectMappings, r.env.resolved, r.env.RepositoryIdentity, nil)
	// A proposed root whose checkout has no repository key cannot own a
	// recorded key; it must not block recovery into configured projects.
	r.recovery.Proposed = proposedRoots(r, projects)
	r.recovery.MaxOperations = 1024
	r.recovery.Validate = r.env.RepositoryIdentityCurrent
	r.recovery.ResetValidationContext(ctx)
	r.gapProjects = projects
	r.gapRecovery = map[bool]*sourcefacts.RecoveryResolver{}
	// Ordinary path ownership still uses committed r.cfg exclusively.
	r.cache = map[string]resolution{}
}

func proposedRoots(r *resolver, projects []archive.ProjectActivation) map[string]bool {
	configured := map[string]bool{}
	for _, p := range r.cfg.Archive.Projects {
		configured[p.Root] = true
	}
	proposed := map[string]bool{}
	for _, p := range projects {
		if !configured[p.Root] {
			proposed[p.Root] = true
		}
	}
	return proposed
}

type recoveryWitnessValidation struct {
	ctx     context.Context
	checked bool
	current bool
}

// recoveryGap is one reason the witness inventory may omit a clone root.
// Agent names the app whose evidence is missing ("" when not attributable);
// the plan reports it only as an app name (RecoveryEvidenceGap).
type recoveryGap struct {
	Agent string
	Cause DiagnosticDetail
}

// recoveryGaps is the bounded set of witness evidence gaps for one plan:
// missing evidence could name a second clone holding the same recorded
// repository key. Which sessions a gap blocks is per app (see blocking).
type recoveryGaps map[recoveryGap]bool

// recoveryGapCauses orders causes for the single diagnostic a candidate
// carries. Budget comes first because it also selects the budget outcome.
var recoveryGapCauses = []DiagnosticDetail{
	CauseRecoveryBudget,
	CauseWitnessLimit,
	CauseNativeStoreUnreadable,
	CauseCursorDatabaseUnavailable,
	CauseNativeInventoryChanged,
	CauseCursorChatFolderUnavailable,
	CauseSessionFolderUnknown,
}

func (g *recoveryGaps) add(agent string, cause DiagnosticDetail) {
	if *g == nil {
		*g = recoveryGaps{}
	}
	(*g)[recoveryGap{Agent: agent, Cause: cause}] = true
}

func (g *recoveryGaps) merge(other recoveryGaps) {
	for gap := range other {
		g.add(gap.Agent, gap.Cause)
	}
}

func (g *recoveryGaps) addUnreadable(unread unreadable) {
	for agent := range unread.folderAgents {
		g.add(agent, CauseNativeStoreUnreadable)
	}
	if unread.folders > 0 && len(unread.folderAgents) == 0 {
		g.add("", CauseNativeStoreUnreadable)
	}
	for agent, unreadable := range unread.stores {
		if unreadable {
			g.add(agent, CauseNativeStoreUnreadable)
		}
	}
}

// cause is the highest-priority gap cause, or "" for a complete inventory.
func (g *recoveryGaps) cause() DiagnosticDetail {
	for _, cause := range recoveryGapCauses {
		for gap := range *g {
			if gap.Cause == cause {
				return cause
			}
		}
	}
	return ""
}

// witnessGap classifies an item that cannot be counted as a witness. ok is
// false for a usable item. A Cursor database chat with no folder evidence at
// all names no root, so it is a non-witness rather than a gap.
func witnessGap(w *work) (cause DiagnosticDetail, ok bool) {
	database := w.c.SourceKind == archive.SourceKindCursorSQLite
	switch {
	case w.appendedOnly() && !database && !w.unsafe && !w.t.identityMismatch && w.res.skip != SkipProjectUnknown:
		// A pure append cannot change header facts; the file stays clone
		// evidence (non-eligible) instead of hiding every app's recoveries.
		return "", false
	case w.vanished || w.sourceChanged:
		if database {
			return CauseCursorDatabaseUnavailable, true
		}
		return CauseNativeInventoryChanged, true
	case w.unsafe || w.t.identityMismatch:
		if database {
			return CauseCursorDatabaseUnavailable, true
		}
		return CauseSessionFolderUnknown, true
	case w.res.skip != SkipProjectUnknown:
		return "", false
	case !database:
		return CauseSessionFolderUnknown, true
	case cursorChatFolderless(w.chat, w.messageFolders):
		return "", false
	default:
		return CauseCursorChatFolderUnavailable, true
	}
}

// cursorChatFolderless reports a Cursor database chat with no folder evidence:
// no folder, no workspace reference and no message folder. Such a chat (for
// example a subagent composer) cannot name a clone root, so it cannot hide a
// second clone. A workspace reference stays evidence even when unreadable, and
// several message folders stay evidence even though none is chosen.
func cursorChatFolderless(chat CursorDatabaseChat, messageFolders []string) bool {
	return chat.Folder == "" && chat.WorkspaceID == "" && len(messageFolders) == 0
}

func recoveryWitnessInventory(ctx context.Context, r *resolver, items []*work) ([]archive.ProjectActivation, map[string][]*work, recoveryGaps) {
	projects := slices.Clone(r.cfg.Archive.Projects)
	configured := map[string]bool{}
	for _, p := range projects {
		configured[r.env.resolved(p.Root)] = true
	}
	observed := map[string]bool{}
	witnesses := map[string][]*work{}
	var gaps recoveryGaps
	if r.inventoryCurrent != nil && !r.inventoryCurrent(ctx) {
		gaps.add("", CauseNativeInventoryChanged)
	}
	for _, w := range items {
		if cause, gap := witnessGap(w); gap {
			gaps.add(string(w.t.harness), cause)
			continue
		}
		if w.res.skip == SkipProjectUnknown {
			continue // folderless: a non-witness, not a gap
		}
		if w.res.skip != "" || w.res.kind != ProjectKindRepository || !r.env.exists(w.res.root) || configured[r.env.resolved(w.res.root)] {
			continue
		}
		// Pending or oversized sources can establish clone uncertainty, but cannot
		// authorize a new destination on their own.
		if _, existing := observed[w.res.root]; !existing && len(observed) >= 1024 {
			gaps.add("", CauseWitnessLimit)
			continue
		}
		// Appended sources and an incomplete source's witnesses count as
		// possible clones but cannot propose a destination.
		eligible := !w.t.capturePending && !w.tooLarge && !w.sourceChanged && !w.evidenceOnly
		observed[w.res.root] = observed[w.res.root] || eligible
		witnesses[w.res.root] = append(witnesses[w.res.root], w)
	}
	for root, eligible := range observed {
		projects = append(projects, archive.ProjectActivation{Root: root, ProjectID: archive.ProjectID(root), Included: eligible})
	}
	return projects, witnesses, gaps
}

func recoveryWitnessesCurrent(ctx context.Context, r *resolver, witnesses map[string][]*work, selectedRoots map[string]bool) bool {
	if r.inventoryCurrent != nil && !r.inventoryCurrent(ctx) {
		return false
	}
	if r.databaseRecoveryCurrent != nil && !r.databaseRecoveryCurrent(ctx) {
		return false
	}
	checks := 0
	ownership := newResolver(r.env, r.cfg, r.filters)
	for root, group := range witnesses {
		found := false
		for _, w := range group {
			if selectedRoots[root] && !w.importable() {
				continue
			}
			if checks >= 1024 || ctx.Err() != nil {
				return false
			}
			checks++
			if w.t.cwd != "" {
				fresh := ownership.resolve(w.t.cwd)
				if fresh.root != w.res.root || fresh.kind != w.res.kind || fresh.skip != w.res.skip {
					continue
				}
			}
			if (w.c.SourceKind == archive.SourceKindCursorSQLite || w.sourceCurrent(r.env) || w.stillAppendedOnly(r.env)) && (w.workspaceCurrent == nil || w.workspaceCurrent(ctx)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// bindRecoveryPolicy binds admitted imports to the exact prospective config,
// while Context and the validation closure retain all hidden clone evidence.
func bindRecoveryPolicy(cfg config.Config, p *Plan) error {
	projected := cfg
	projected.Archive.Projects = slices.Clone(cfg.Archive.Projects)
	projected.ImportedHarnesses = slices.Clone(cfg.ImportedHarnesses)
	if _, err := ApplyToConfig(&projected, *p, p.GeneratedAt); err != nil {
		return err
	}
	policy := sourcefacts.RecoveryContext(projected.Archive.Projects, nil, filepath.Clean)
	for i := range p.Candidates {
		c := &p.Candidates[i]
		if c.ProjectResolution != nil {
			c.ProjectResolution.PolicyContext = policy
		}
	}
	return nil
}

// CheckRecovery requires a non-nil context and renews recovered ownership
// before committing proposed capture configuration. It performs repository work outside the short hooks lock hold.
func (p Plan) CheckRecovery(ctx context.Context) error {
	if ctx == nil {
		return errors.New("recovery validation requires a context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, c := range p.Imported() {
		if c.projectResolutionReset != nil {
			c.projectResolutionReset(ctx)
		}
	}
	for _, c := range p.Imported() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.projectResolutionCurrent != nil && !c.projectResolutionCurrent() {
			return errors.New("project or source evidence changed or is unavailable; run backfill again. Nothing was changed")
		}
	}
	return ctx.Err()
}
