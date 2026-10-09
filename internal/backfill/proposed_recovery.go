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
func prepareRecoveryInventory(ctx context.Context, r *resolver, items []*work, unread unreadable, dbIncomplete bool) {
	projects, witnesses, incomplete := recoveryWitnessInventory(ctx, r, items)
	incomplete = incomplete || dbIncomplete || unread.folders > 0 || len(unread.stores) > 0
	selectedRoots := map[string]bool{}
	r.requireWitnessFormats = func(root string) {
		selectedRoots[root] = true
		for _, w := range witnesses[root] {
			w.proposedWitness = true
		}
	}
	r.proposedRootEligible = func(root string) bool {
		for _, w := range witnesses[root] {
			if w.validated && w.importable() && !w.vanished {
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
	lookup := r.env.RepositoryIdentity
	if incomplete {
		lookup = func(context.Context, string) sourcefacts.RepositoryIdentity {
			return sourcefacts.RepositoryIdentity{BudgetExhausted: r.recoveryInventoryBudget}
		}
	}
	r.mappingRecovery = r.recovery
	r.recovery = sourcefacts.NewRecoveryResolver(projects, r.filters.ProjectMappings, r.env.resolved, lookup, nil)
	if !incomplete {
		// A proposed root whose checkout has no repository key cannot own a
		// recorded key; it must not block recovery into configured projects.
		r.recovery.Proposed = proposedRoots(r, projects)
	}
	r.recovery.MaxOperations = 1024
	r.recovery.Validate = r.env.RepositoryIdentityCurrent
	r.recovery.ResetValidationContext(ctx)
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

func recoveryWitnessInventory(ctx context.Context, r *resolver, items []*work) ([]archive.ProjectActivation, map[string][]*work, bool) {
	projects := slices.Clone(r.cfg.Archive.Projects)
	configured := map[string]bool{}
	for _, p := range projects {
		configured[r.env.resolved(p.Root)] = true
	}
	observed := map[string]bool{}
	witnesses := map[string][]*work{}
	incomplete := r.inventoryCurrent != nil && !r.inventoryCurrent(ctx)
	for _, w := range items {
		if w.vanished || w.sourceChanged || w.unsafe || w.t.identityMismatch {
			incomplete = true
			continue
		}
		if w.res.skip == SkipProjectUnknown {
			incomplete = true
			continue
		}
		if w.res.skip != "" || w.res.kind != ProjectKindRepository || !r.env.exists(w.res.root) || configured[r.env.resolved(w.res.root)] {
			continue
		}
		// Pending or oversized sources can establish clone uncertainty, but cannot
		// authorize a new destination on their own.
		if _, existing := observed[w.res.root]; !existing && len(observed) >= 1024 {
			incomplete = true
			continue
		}
		eligible := !w.t.capturePending && !w.tooLarge
		observed[w.res.root] = observed[w.res.root] || eligible
		witnesses[w.res.root] = append(witnesses[w.res.root], w)
	}
	for root, eligible := range observed {
		projects = append(projects, archive.ProjectActivation{Root: root, ProjectID: archive.ProjectID(root), Included: eligible})
	}
	return projects, witnesses, incomplete
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
			if (w.c.SourceKind == archive.SourceKindCursorSQLite || w.sourceCurrent(r.env)) && (w.workspaceCurrent == nil || w.workspaceCurrent(ctx)) {
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
