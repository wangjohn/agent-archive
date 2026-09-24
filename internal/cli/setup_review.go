package cli

import (
	"fmt"
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

// reviewAction asks the final confirmation, returning start, edit, or cancel.
// y, n, and e still work for scripted input.
func reviewAction(p *prompter, reconfiguring bool) (string, error) {
	label, yes := "Start archiving?", "Yes, start archiving"
	if reconfiguring {
		label, yes = "Save these changes?", "Yes, save"
	}
	choice, err := p.menu("\n"+label, "yes",
		option{"yes", yes},
		option{"edit", "Edit a setting"},
		option{"no", "Cancel (your setup draft is kept)"})
	switch choice {
	case "yes":
		return "start", err
	case "no":
		return "cancel", err
	}
	return choice, err
}

func editSetupReview(p *prompter, draft *setupDraft, userHome string) error {
	choices := []option{
		{"apps", "Apps to include"},
		{"projects", "Projects to include"},
		{"sessions", "All sessions or only sessions using skills"},
		{"retention", "How long sessions are kept"},
		{"storage", "Bucket or credentials"},
		{"prefix", "Folder inside the bucket"},
	}
	if draft.Config.Storage.Provider == credentials.ProviderS3 {
		choices = append(choices, option{"region", "AWS bucket region"})
	}
	choices = append(choices, option{"back", "Nothing, go back to the review"})
	choice, err := p.menu("\nWhat would you like to change?", "back", choices...)
	if err != nil {
		return err
	}
	switch choice {
	case "apps":
		draft.Config.Harnesses, err = promptHarnesses(p, nil, draft.Config.Harnesses)
	case "projects":
		projects, e := promptProjects(p, draft.Config.Archive.Projects, time.Time{}, userHome)
		if e != nil {
			return e
		}
		if len(projects) == 0 {
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
