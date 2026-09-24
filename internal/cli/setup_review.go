package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func showSetupReview(p *prompter, cfg config.Config, reconfiguring bool, discoveries map[string]applicationDiscovery) {
	title := "Ready to start"
	if reconfiguring {
		title = "Review your changes"
	}
	fmt.Fprintln(p.out, "\n3 of 3 — "+title+"\n")
	fmt.Fprintf(p.out, "Apps       %s\n", friendlyApps(cfg.Harnesses))
	for _, app := range cfg.Harnesses {
		profile := captureCapabilityProfile(app)
		discovery := discoveries[app]
		version := discovery.Version
		if version == "" {
			version = discovery.VersionState
		}
		if version == "" {
			version = "unknown"
		}
		fmt.Fprintf(p.out, "  %s: installed version %s (capture unverified); fresh-start evidence %s; transcript %s\n", appName(app), version, profile.FreshStart.State, profile.Transcript.State)
	}
	if len(cfg.ImportedHarnesses) > 0 {
		fmt.Fprintf(p.out, "Imported only: %s (sessions imported by backfill are published; new sessions are not captured)\n", friendlyApps(cfg.ImportedHarnesses))
	}
	for _, project := range cfg.Archive.Projects {
		if project.Included {
			fmt.Fprintf(p.out, "Project    %s\n", project.Root)
		}
	}
	provider := "Cloudflare R2"
	if cfg.Storage.Provider == credentials.ProviderS3 {
		provider = "Amazon S3"
	}
	fmt.Fprintf(p.out, "Storage    %s · %s\n", provider, cfg.Storage.Bucket)
	if cfg.Storage.Provider == credentials.ProviderS3 {
		fmt.Fprintf(p.out, "AWS        %s · %s\n", cfg.Storage.AWSProfile, cfg.Storage.Region)
	}
	if cfg.Storage.Prefix != defaultPrefix {
		fmt.Fprintf(p.out, "Folder     %s\n", cfg.Storage.Prefix)
	}
	if cfg.RequireSkillUse {
		fmt.Fprintln(p.out, "\nSave new sessions with detected skill use.")
	} else {
		fmt.Fprintln(p.out, "\nSave new sessions, with or without skills.")
	}
	fmt.Fprintf(p.out, "Automatically delete archived sessions after %d days.\n", cfg.RetentionDays)
	fmt.Fprintln(p.out, "Filtering is best effort; sensitive text may remain.")
	printReviewPrivacy(p, cfg)
}

// printReviewPrivacy reports the saved bucket privacy evidence as of the
// prompter's clock, so the review screen agrees with status output.
func printReviewPrivacy(p *prompter, cfg config.Config) {
	printBucketPrivacy(p.out, currentBucketPrivacy(cfg, p.clock()))
}

func reviewAction(p *prompter, label string) (string, error) {
	for {
		answer, err := p.line(label + " [Y/n/edit] ")
		if err != nil {
			return "", err
		}
		switch strings.ToLower(answer) {
		case "", "y", "yes":
			return "start", nil
		case "n", "no":
			return "cancel", nil
		case "edit", "e":
			return "edit", nil
		default:
			fmt.Fprintln(p.out, "Enter y to continue, n to cancel, or edit to change something.")
		}
	}
}

// promptStopImported offers to stop publishing each app that has only
// imported sessions: one backfill imported without its hooks installed, and
// which setup is not installing hooks for now. Stopping removes it from
// ImportedHarnesses when setup commits, so its imports are no longer
// published; sessions already in the bucket stay until retention removes
// them.
func promptStopImported(p *prompter, draft *setupDraft) error {
	for _, app := range draft.Config.ImportedHarnesses {
		if containsString(draft.Config.Harnesses, app) || containsString(draft.StopImported, app) {
			continue
		}
		keep, err := p.yesNo(fmt.Sprintf("Keep publishing %s sessions imported by backfill?", appName(app)), true)
		if err != nil {
			return err
		}
		if !keep {
			draft.StopImported = append(draft.StopImported, app)
		}
	}
	return nil
}

// offerStopImported refreshes the draft's list from the committed
// configuration, which is where backfill writes it, then prompts.
func offerStopImported(p *prompter, draft *setupDraft, committed config.Config) error {
	draft.Config.ImportedHarnesses = carriedImportedHarnesses(committed.ImportedHarnesses, draft.Config.Harnesses, draft.StopImported)
	return promptStopImported(p, draft)
}

func editSetupReview(p *prompter, draft *setupDraft, userHome string, backfilled map[string]bool) error {
	fmt.Fprintln(p.out, "\nWhat would you like to change?")
	fmt.Fprintln(p.out, "  apps       Which apps to include\n  projects   Which projects to include\n  sessions   All sessions or only sessions using skills\n  retention  How long sessions are kept\n  storage    Bucket or credentials\n  prefix     Folder inside the bucket")
	choices := []string{"apps", "projects", "sessions", "retention", "storage", "prefix", "back"}
	if draft.Config.Storage.Provider == credentials.ProviderS3 {
		fmt.Fprintln(p.out, "  region     AWS bucket region")
		choices = append(choices, "region")
	}
	fmt.Fprintln(p.out, "  back       Return to review")
	choice, err := promptChoice(p, "Change", "back", choices...)
	if err != nil {
		return err
	}
	switch choice {
	case "apps":
		draft.Config.Harnesses, err = promptHarnesses(p, nil, draft.Config.Harnesses)
		if err != nil {
			return err
		}
		err = promptStopImported(p, draft)
	case "projects":
		projects, e := promptProjects(p, draft.Config.Archive.Projects, backfilled, time.Time{}, userHome)
		if e != nil {
			return e
		}
		if includedProjects(projects) == 0 {
			fmt.Fprintln(p.out, "At least one project is needed. Your previous selection is kept.")
		} else {
			draft.Config.Archive.Projects = projects
		}
	case "sessions":
		all, e := p.yesNo("Save sessions even when no skills are used?", !draft.Config.RequireSkillUse)
		if e != nil {
			return e
		}
		draft.Config.RequireSkillUse = !all
	case "retention":
		draft.Config.RetentionDays, err = p.intWithDefault("Keep sessions for how many days?", draft.Config.RetentionDays)
	case "storage":
		draft.Step = 1
	case "prefix":
		for {
			prefix, e := p.required("Folder inside the bucket", draft.Config.Storage.Prefix)
			if e != nil {
				return e
			}
			if _, e = storage.Prefix(prefix, "test"); e == nil {
				draft.Config.Storage.Prefix = prefix
				break
			}
			fmt.Fprintln(p.out, "Use a relative folder name, such as agent-archive/; do not include .. or a leading slash.")
		}
	case "region":
		draft.Config.Storage.Region, err = p.required("Bucket region", draft.Config.Storage.Region)
	}
	return err
}
