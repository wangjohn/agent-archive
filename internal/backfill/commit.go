package backfill

import (
	"fmt"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// CheckClock refuses an admission time earlier than the plan, or than the
// boundaries the import's sessions are admitted across: the current
// destination, and each configured project they land in. A clock set back
// would otherwise admit sessions that start after their admission, or that
// the configuration then rejects, silently.
func CheckClock(cfg config.Config, p Plan, admittedAt time.Time) error {
	// The plan judged start_in_future by its own time; an admission before
	// it could admit a session that starts after its admission.
	if !p.GeneratedAt.IsZero() && admittedAt.Before(p.GeneratedAt) {
		return fmt.Errorf("the clock reads %s, earlier than when this plan was made (%s); check the clock and run backfill again",
			admittedAt.Format(time.RFC3339), p.GeneratedAt.Format(time.RFC3339))
	}
	if !cfg.DestinationSince.IsZero() && admittedAt.Before(cfg.DestinationSince) {
		return fmt.Errorf("the clock reads %s, earlier than when this storage destination was set up (%s); check the clock and run backfill again",
			admittedAt.Format(time.RFC3339), cfg.DestinationSince.Format(time.RFC3339))
	}
	targets := map[string]bool{}
	for _, c := range p.Imported() {
		targets[c.ProjectRoot] = true
	}
	for _, project := range cfg.Archive.Projects {
		if project.Included && targets[project.Root] && admittedAt.Before(project.ActivatedAt) {
			return fmt.Errorf("the clock reads %s, earlier than when %s was added (%s); check the clock and run backfill again",
				admittedAt.Format(time.RFC3339), p.display(project.Root), project.ActivatedAt.Format(time.RFC3339))
		}
	}
	return nil
}

// ApplyToConfig adds what the plan needs to cfg: every project it imports
// into that is not configured yet, included and activated at admittedAt, and
// every app it imports that has no hooks to ImportedHarnesses. It returns the
// added projects' IDs and the added apps.
func ApplyToConfig(cfg *config.Config, p Plan, admittedAt time.Time) (projectIDs, apps []string) {
	projectIDs, apps = []string{}, []string{}
	for _, c := range p.Imported() {
		if c.ProjectIncluded || slices.ContainsFunc(cfg.Archive.Projects, func(existing archive.ProjectActivation) bool { return existing.Root == c.ProjectRoot }) {
			continue
		}
		id := archive.ProjectID(c.ProjectRoot)
		cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{
			ProjectID: id, Root: c.ProjectRoot, ActivatedAt: admittedAt.UTC(), Included: true,
		})
		projectIDs = append(projectIDs, id)
	}
	for _, app := range p.AppsWithoutHooks() {
		if !slices.Contains(cfg.ImportedHarnesses, app) {
			cfg.ImportedHarnesses = append(cfg.ImportedHarnesses, app)
			apps = append(apps, app)
		}
	}
	return projectIDs, apps
}
