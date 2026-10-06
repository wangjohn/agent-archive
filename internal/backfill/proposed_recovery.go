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
func prepareRecoveryInventory(ctx context.Context, r *resolver, items []*work, unread unreadable) {
	projects := slices.Clone(r.cfg.Archive.Projects)
	configured := map[string]bool{}
	for _, p := range projects {
		configured[r.env.resolved(p.Root)] = true
	}
	observed := map[string]bool{}
	witnesses := map[string][]*work{}
	incomplete := unread.folders > 0 || len(unread.stores) > 0
	for _, w := range items {
		if w.vanished || w.sourceChanged || w.unsafe || w.t.identityMismatch {
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
	r.requireWitnessFormats = func(root string) {
		for _, w := range witnesses[root] {
			if w.t.cursorSlug != "" {
				w.proposedWitness = true
			}
		}
	}
	r.proposedRootEligible = func(root string) bool {
		for _, w := range witnesses[root] {
			if !w.t.capturePending && !w.tooLarge && !w.unsafe && !w.empty && !w.t.identityMismatch && !w.sourceChanged && !w.vanished {
				return true
			}
		}
		return false
	}
	checkedSources, currentSources := false, false
	validationCtx := ctx
	r.recoverySourcesReset = func(ctx context.Context) {
		checkedSources, currentSources = false, false
		validationCtx = ctx
		if r.workspaceReset != nil {
			r.workspaceReset()
		}
	}
	r.recoverySourcesCurrent = func() bool {
		if checkedSources {
			return currentSources
		}
		checkedSources = true
		checks := 0
		for _, group := range witnesses {
			found := false
			for _, w := range group {
				if checks >= 1024 || validationCtx.Err() != nil {
					return false
				}
				checks++
				if w.sourceCurrent(r.env) && (w.workspaceCurrent == nil || w.workspaceCurrent(validationCtx)) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		currentSources = true
		return true
	}
	lookup := r.env.RepositoryIdentity
	if incomplete && len(observed) > 0 {
		lookup = nil
	}
	r.recovery = sourcefacts.NewRecoveryResolver(projects, r.filters.ProjectMappings, r.env.resolved, lookup, nil)
	r.recovery.MaxOperations = 1024
	r.recovery.Validate = r.env.RepositoryIdentityCurrent
	r.recovery.ResetValidationContext(ctx)
	// Ordinary path ownership still uses committed r.cfg exclusively.
	r.cache = map[string]resolution{}
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

// CheckRecovery renews recovered ownership before committing proposed capture
// configuration. It performs repository work outside the short hooks lock hold.
func (p Plan) CheckRecovery(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
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
