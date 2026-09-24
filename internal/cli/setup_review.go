package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// reviewRow is one labeled line of the review summary. A row with several
// values, such as projects, prints one value per line.
type reviewRow struct {
	label  string
	values []string
}

// reviewRows lists what setup will save, in the order the review shows it.
func reviewRows(cfg config.Config, discoveries map[string]applicationDiscovery) []reviewRow {
	var apps []string
	for _, app := range cfg.Harnesses {
		apps = append(apps, appWithVersion(app, discoveries[app]))
	}
	var projects []string
	for _, project := range cfg.Archive.Projects {
		if project.Included {
			projects = append(projects, project.Root)
		}
	}
	sessions := "All new sessions, with or without skills"
	if cfg.RequireSkillUse {
		sessions = "Only new sessions that use skills"
	}
	rows := []reviewRow{
		{"Apps", []string{strings.Join(apps, ", ")}},
		{"Projects", projects},
		{"Sessions", []string{sessions}},
		{"Keep for", []string{fmt.Sprintf("%d days, then deleted automatically", cfg.RetentionDays)}},
	}
	s := cfg.Storage
	if s.Provider == credentials.ProviderS3 {
		rows = append(rows, reviewRow{"Storage", []string{"Amazon S3"}}, reviewRow{"Bucket", []string{s.Bucket}}, reviewRow{"AWS", []string{"profile " + s.AWSProfile + " · " + s.Region}})
	} else {
		rows = append(rows, reviewRow{"Storage", []string{"Cloudflare R2"}}, reviewRow{"Bucket", []string{s.Bucket}})
		if s.R2AccountID != "" {
			rows = append(rows, reviewRow{"Account", []string{s.R2AccountID}})
		} else {
			rows = append(rows, reviewRow{"Endpoint", []string{s.R2Endpoint}})
		}
	}
	if s.Prefix != "" && s.Prefix != defaultPrefix {
		rows = append(rows, reviewRow{"Folder", []string{s.Prefix}})
	}
	return rows
}

func appWithVersion(app string, discovery applicationDiscovery) string {
	switch {
	case discovery.Version != "":
		return appName(app) + " " + discovery.Version
	case discovery.VersionState == "absent":
		return appName(app) + " (not found)"
	}
	return appName(app) + " (version unknown)"
}

// showSetupReview prints the summary of what will be saved. When
// reconfiguring, a value that differs from the active configuration is
// marked, with the old value beneath it, so only those lines need checking.
func showSetupReview(p *prompter, cfg, existing config.Config, reconfiguring bool, discoveries map[string]applicationDiscovery) {
	title := "Ready to start"
	if reconfiguring {
		title = "Review your changes"
	}
	p.step(3, title)
	before := map[string][]string{}
	if reconfiguring {
		for _, row := range reviewRows(existing, discoveries) {
			before[row.label] = row.values
		}
	}
	changed := 0
	for _, row := range reviewRows(cfg, discoveries) {
		old, had := before[row.label]
		isChanged := reconfiguring && strings.Join(old, "\n") != strings.Join(row.values, "\n")
		mark := "  "
		if isChanged {
			changed++
			mark = p.style.yellow("* ")
		}
		values := row.values
		if len(values) == 0 {
			values = []string{"none"}
		}
		for i, value := range values {
			label := ""
			if i == 0 {
				label = row.label
			}
			if isChanged {
				value = p.style.yellow(value)
			}
			fmt.Fprintf(p.out, "%s%s %s\n", mark, p.style.dim(fmt.Sprintf("%-9s", label)), value)
			mark = "  "
		}
		if isChanged {
			was := "not set"
			if had && len(old) > 0 {
				was = strings.Join(old, ", ")
			}
			fmt.Fprintf(p.out, "  %9s %s\n", "", p.style.dim("was "+was))
		}
	}
	if reconfiguring && changed == 0 {
		fmt.Fprintln(p.out, p.style.dim("\n  Nothing above differs from your current settings."))
	} else if reconfiguring {
		fmt.Fprintln(p.out, p.style.dim("\n  * changed from your current settings"))
	}
}

// printReviewNotes prints the caveats that always apply, after any
// change-specific warnings.
func printReviewNotes(p *prompter, cfg config.Config, discoveries map[string]applicationDiscovery) {
	printReviewPrivacy(p, cfg)
	p.note("Filtering is best effort; sensitive text may remain in archived sessions.")
	var apps []string
	for _, app := range cfg.Harnesses {
		apps = append(apps, appWithVersion(app, discoveries[app]))
	}
	if len(apps) > 0 {
		p.note("Capture with "+strings.Join(apps, ", ")+" is not confirmed until a session is archived.", "After your first session, check it with agent-archive status.")
	}
}

// printReviewPrivacy reports the saved bucket privacy evidence as of the
// prompter's clock, so the review screen agrees with status output.
func printReviewPrivacy(p *prompter, cfg config.Config) {
	report := currentBucketPrivacy(cfg, p.clock())
	switch report.State {
	case "verified_private":
		p.item(p.style.green("✓"), "Bucket privacy: native public access blocked at the last check.", nil)
	case "public_or_risky":
		p.item(p.style.red("!"), p.style.red("The bucket looks public ("+privacyReasonText(report.Reason)+")."), []string{"Fix its access before archiving: " + report.GuidanceURL})
	default:
		p.warn("Bucket privacy not verified: "+privacyReasonText(report.Reason)+".", "Make sure public access is off: "+report.GuidanceURL)
	}
}

func privacyReasonText(reason string) string {
	switch reason {
	case "r2_management_credentials_not_configured":
		return "R2 storage keys cannot read public-access settings"
	case "inspection_unavailable":
		return "public-access settings could not be read"
	case "public_access_controls_not_fully_verified":
		return "not every public-access setting could be confirmed"
	case "inspection_stale":
		return "the last check is more than a day old"
	case "storage_configuration_changed":
		return "storage changed since the last check"
	case "public_bucket_policy":
		return "a bucket policy allows public access"
	case "public_bucket_acl":
		return "the bucket ACL allows public access"
	}
	return strings.ReplaceAll(reason, "_", " ")
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

func editSetupReview(p *prompter, draft *setupDraft, userHome string) error {
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
