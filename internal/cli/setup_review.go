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
	}
	if len(cfg.ImportedHarnesses) > 0 {
		rows = append(rows, reviewRow{"Imported", []string{friendlyApps(cfg.ImportedHarnesses) + " (sessions imported by backfill stay published; new sessions are not captured)"}})
	}
	rows = append(rows, []reviewRow{
		{"Projects", projects},
		{"Sessions", []string{sessions}},
		{"Keep for", []string{fmt.Sprintf("%d days, then deleted automatically", cfg.RetentionDays)}},
	}...)
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
