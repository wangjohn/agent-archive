package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// setupReview is what the review screen shows beside the configuration
// being saved.
type setupReview struct {
	// existing is the active configuration, which a reconfiguration's
	// changes are marked against.
	existing      config.Config
	reconfiguring bool
	discoveries   map[string]applicationDiscovery
	// checks are the checks setup made before its first question; the hook
	// files' results feed the checklist.
	checks preflightChecks
	// hookFiles are the files each app's hooks go into.
	hookFiles hooks.Files
	userHome  string
}

// reviewRow is one labeled line of the review summary. A row with several
// values, such as projects, prints one value per line. Detail follows the
// first value, dim: secondary facts, such as the bucket's region.
type reviewRow struct {
	label  string
	values []string
	detail string
}

// text is the row as one string, for comparing and for "was".
func (r reviewRow) text() string {
	text := strings.Join(r.values, ", ")
	if r.detail != "" {
		text += "  " + r.detail
	}
	return text
}

// reviewRows lists what setup will save, in the order the review shows it.
// The Sessions row appears only when showSessions is set: it is noise while
// every new session is saved, the default.
func reviewRows(cfg config.Config, discoveries map[string]applicationDiscovery, showSessions bool, userHome string) []reviewRow {
	var apps []string
	for _, app := range cfg.Harnesses {
		apps = append(apps, appWithVersion(app, discoveries[app]))
	}
	var projects []string
	for _, project := range cfg.Archive.Projects {
		if project.Included {
			projects = append(projects, displayPath(project.Root, userHome))
		}
	}
	sessions := "All new sessions, with or without skills"
	if cfg.RequireSkillUse {
		sessions = "Only new sessions that use skills"
	}
	rows := []reviewRow{
		{label: "Apps", values: []string{strings.Join(apps, " · ")}},
	}
	if len(cfg.DeclinedHarnesses) > 0 {
		// An app leaves this list only by being included, which changes the
		// Apps row, so the row need not appear when the list is empty.
		rows = append(rows, reviewRow{label: "Skipped", values: []string{appList(cfg.DeclinedHarnesses) + " (setup will not offer again; to add back, choose Apps and projects in agent-archive setup)"}})
	}
	if len(cfg.ImportedHarnesses) > 0 {
		rows = append(rows, reviewRow{label: "Imported", values: []string{friendlyApps(cfg.ImportedHarnesses) + " (sessions imported by backfill stay published; new sessions are not captured)"}})
	}
	rows = append(rows, reviewRow{label: "Projects", values: projects})
	if showSessions {
		rows = append(rows, reviewRow{label: "Sessions", values: []string{sessions}})
	}
	address, detail := storageAddress(cfg.Storage)
	rows = append(rows, reviewRow{label: "Storage", values: []string{address}, detail: detail})
	days := fmt.Sprintf("%d days", cfg.RetentionDays)
	if cfg.RetentionDays == 1 {
		days = "1 day"
	}
	return append(rows, reviewRow{label: "Keep for", values: []string{days}})
}

// storageAddress is where sessions are stored, as one address such as
// s3://bucket/agent-archive/, with what else locates it: the S3 region and
// profile, or the R2 account or endpoint.
func storageAddress(s credentials.Config) (address, detail string) {
	prefix := strings.TrimSuffix(firstNonEmpty(s.Prefix, defaultPrefix), "/") + "/"
	if s.Provider == credentials.ProviderS3 {
		return "s3://" + s.Bucket + "/" + prefix, s.Region + " · profile " + firstNonEmpty(s.AWSProfile, "default")
	}
	if s.R2AccountID != "" {
		return "r2://" + s.Bucket + "/" + prefix, "account " + s.R2AccountID
	}
	return "r2://" + s.Bucket + "/" + prefix, "endpoint " + s.R2Endpoint
}

func appWithVersion(app string, discovery applicationDiscovery) string {
	switch {
	case discovery.Version != "":
		return appName(app) + " " + discovery.Version
	case discovery.VersionState == "absent":
		return appName(app) + " (not found)"
	}
	return appName(app) + " (version not detected)"
}

// reviewDiscoveries returns discoveries as the review shows them. An app
// discovery could not find but whose config directory detection saw is
// reported as installed with an unknown version rather than "not found":
// its CLI may live somewhere discovery does not look. Recorded discoveries
// keep the absent state, which version support relies on.
func reviewDiscoveries(discoveries map[string]applicationDiscovery, detected []string) map[string]applicationDiscovery {
	result := make(map[string]applicationDiscovery, len(discoveries))
	for app, discovery := range discoveries {
		if discovery.VersionState == "absent" && containsString(detected, app) {
			discovery = applicationDiscovery{Installed: true, VersionState: "unknown"}
		}
		result[app] = discovery
	}
	return result
}

// showSetupReview prints the summary of what will be saved, then the
// checklist of what setup found. When reconfiguring, a value that differs
// from the active configuration is marked, with the old value beneath it,
// so only those lines need checking.
func showSetupReview(p *prompter, cfg config.Config, review setupReview) {
	title := "Ready to start"
	if review.reconfiguring {
		title = "Review your changes"
	}
	p.step(3, title)
	changed := printReviewRows(p, cfg, review)
	if review.reconfiguring && changed == 0 {
		terminal.Println(p.out, p.style.dim("\n  Nothing above differs from your current settings."))
	} else if review.reconfiguring {
		terminal.Println(p.out, p.style.dim("\n  * changed from your current settings"))
	}
	terminal.Println(p.out, "")
	printReviewChecklist(p, reviewChecklist(cfg, review, p.clock()))
}

// printReviewRows prints the summary rows and returns how many changed.
// Only the change mark is colored; a label is dim, as is the old value.
func printReviewRows(p *prompter, cfg config.Config, review setupReview) int {
	existing := review.existing
	// A change back to saving every session still shows, as a change.
	showSessions := cfg.RequireSkillUse || review.reconfiguring && existing.RequireSkillUse
	before := map[string]reviewRow{}
	if review.reconfiguring {
		for _, row := range reviewRows(existing, review.discoveries, showSessions, review.userHome) {
			before[row.label] = row
		}
	}
	changed := 0
	for _, row := range reviewRows(cfg, review.discoveries, showSessions, review.userHome) {
		old, had := before[row.label]
		isChanged := review.reconfiguring && old.text() != row.text()
		mark := "  "
		if isChanged {
			changed++
			mark = p.style.warn("*") + " "
		}
		values := row.values
		if len(values) == 0 {
			values = []string{"none"}
		}
		for i, value := range values {
			label := ""
			if i == 0 {
				label = row.label
				if row.detail != "" {
					value += "  " + p.style.dim(row.detail)
				}
			}
			terminal.Printf(p.out, "%s%s %s\n", mark, p.style.dim(fmt.Sprintf("%-10s", label)), value)
			mark = "  "
		}
		if isChanged {
			was := "not set"
			if had && len(old.values) > 0 {
				was = old.text()
			}
			terminal.Printf(p.out, "  %10s %s\n", "", p.style.dim("was "+was))
		}
	}
	return changed
}

// reviewCheck is one line of the review's checklist: a ✓, ! or ✗, what was
// checked, and what was found, with any further lines under it. A link
// ends the last of those lines.
type reviewCheck struct {
	mark   string
	label  string
	detail string
	more   []string
	link   string
}

// reviewChecklist is what the review found, for cfg: the storage check,
// the bucket's privacy as of at, the hook files, and the step each newly
// included app needs after setup. The review follows a storage check that
// passed, so storage is connected.
func reviewChecklist(cfg config.Config, review setupReview, at time.Time) []reviewCheck {
	checks := []reviewCheck{
		{mark: symbolOK, label: "Storage connected", detail: "write, read, list, delete"},
		privacyCheck(cfg, at),
	}
	if hooksCheck, ok := hookFilesCheck(cfg.Harnesses, review.checks, review.hookFiles, review.userHome); ok {
		checks = append(checks, hooksCheck)
	}
	for _, app := range cfg.Harnesses {
		// An app whose hooks are installed already has taken its step.
		if review.reconfiguring && containsString(review.existing.Harnesses, app) {
			continue
		}
		if step, ok := appStepAfterSetup[app]; ok {
			checks = append(checks, reviewCheck{mark: symbolWarn, label: appName(app) + " needs one step", detail: step})
		}
	}
	return checks
}

// appStepAfterSetup is what an app needs from the user, after setup, before
// it runs the archive hooks. Apps not listed need nothing.
var appStepAfterSetup = map[string]string{
	"codex": "approve the hooks with /hooks after setup",
}

// privacyCheck reports the saved bucket privacy evidence as of at, so the
// review agrees with status output.
func privacyCheck(cfg config.Config, at time.Time) reviewCheck {
	report := currentBucketPrivacy(cfg, at)
	switch {
	case report.State == "verified_private":
		return reviewCheck{mark: symbolOK, label: "Bucket is private", detail: "all public access blocked"}
	case report.State == "public_or_risky":
		return reviewCheck{mark: symbolFail, label: "Bucket is public", detail: privacyReasonText(report.Reason), more: []string{"Fix its access before archiving:"}, link: report.GuidanceURL}
	case report.Reason == r2PrivacyUnreadable:
		// Every R2 bucket reads this way, and nothing setup can do changes
		// it, so it is a reminder of what to check.
		return reviewCheck{mark: symbolWarn, label: "Bucket privacy unknown", detail: "check public access in the Cloudflare dashboard"}
	}
	return reviewCheck{mark: symbolWarn, label: "Bucket privacy unknown", detail: privacyReasonText(report.Reason), more: []string{"Make sure public access is off:"}, link: report.GuidanceURL}
}

// hookFilesCheck reports on the hook files of apps, from the checks setup
// made before its first question. An app chosen since, which those checks
// did not cover, has its file checked now. It reports nothing without apps.
func hookFilesCheck(apps []string, checks preflightChecks, files hooks.Files, userHome string) (reviewCheck, bool) {
	var paths []string
	var bad *preflightCheck
	for _, app := range apps {
		check, found := preflightCheck{}, false
		for _, c := range checks {
			if c.App == app {
				check, found = c, true
				break
			}
		}
		if !found {
			check = checkHookFile(app, files[app], userHome)
		}
		if !check.OK && bad == nil {
			bad = &check
		}
		paths = append(paths, check.Detail)
	}
	if len(paths) == 0 {
		return reviewCheck{}, false
	}
	if bad != nil {
		return reviewCheck{mark: symbolFail, label: appName(bad.App) + " hook file is invalid", detail: bad.Detail, more: []string{bad.Problem, "Setup edits only plain JSON. Fix the file before you start."}}, true
	}
	label := "Hook files are valid"
	if len(paths) == 1 {
		label = "Hook file is valid"
	}
	return reviewCheck{mark: symbolOK, label: label, detail: strings.Join(paths, ", ")}, true
}

// checkHookFile checks app's hook file at path, as the checks before the
// first question do.
func checkHookFile(app, path, userHome string) preflightCheck {
	problems := hooks.Validate(hooks.Files{app: path})
	if len(problems) == 0 {
		return preflightCheck{App: app, Detail: displayPath(path, userHome), OK: true}
	}
	where := displayPath(path, userHome)
	if problems[0].Line > 0 {
		where = fmt.Sprintf("%s:%d:%d", where, problems[0].Line, problems[0].Column)
	}
	return preflightCheck{App: app, Detail: where, Problem: sentence(problems[0].Reason)}
}

// printReviewChecklist prints the checklist with its details in a column.
// Only the symbol is colored, and a link, which the user opens.
func printReviewChecklist(p *prompter, checks []reviewCheck) {
	for _, check := range checks {
		mark := p.style.okMark()
		if check.mark == symbolWarn {
			mark = p.style.warnMark()
		} else if check.mark == symbolFail {
			mark = p.style.failMark()
		}
		terminal.Println(p.out, p.style.hang(fmt.Sprintf("  %s %-22s  ", mark, check.label), check.detail))
		for i, line := range check.more {
			if i == len(check.more)-1 && check.link != "" {
				line += " " + p.style.cmd(check.link)
			}
			if line != "" {
				terminal.Println(p.out, p.style.hang("    ", line))
			}
		}
	}
}

// reviewHookFiles warns when an app's hooks will not go where setup
// installed them last time: this shell's CLAUDE_CONFIG_DIR or CODEX_HOME
// differs from the one setup saw then, and confirming moves the hooks.
// recorded is whether the configuration recorded where setup installed
// them; an earlier release did not, and always used the fixed paths.
func reviewHookFiles(p *prompter, apps []string, next, previous hooks.Files, installed []string, recorded bool, userHome string) {
	variable := map[string]string{"claude": "CLAUDE_CONFIG_DIR", "codex": "CODEX_HOME"}
	for _, app := range apps {
		if !containsString(installed, app) || previous[app] == next[app] {
			continue
		}
		reason := variable[app] + " in this shell differs from when setup last ran. To keep them where they are,"
		if !recorded {
			reason = fmt.Sprintf("An earlier release installed them at the fixed path; %s is set in this shell. To keep them there,", variable[app])
		}
		p.warn(fmt.Sprintf("%s hooks move to %s from %s.", appName(app), displayPath(next[app], userHome), displayPath(previous[app], userHome)), reason,
			fmt.Sprintf("cancel and run agent-archive setup from a shell without %s.", variable[app]))
	}
}

// privacyDocURL is the page on what leaves the Mac and what filtering
// removes.
const privacyDocURL = "https://github.com/wangjohn/agent-archive/blob/main/docs/security/privacy.md"

// printReviewNotes prints the caveat that always applies, last and dim.
func printReviewNotes(p *prompter) {
	terminal.Println(p.out, "")
	terminal.Println(p.out, p.style.dim(p.style.hang("  ", "Filtering is best effort; sensitive text may remain in archived sessions.")))
	terminal.Println(p.out, p.style.dim(p.style.hang("  ", "What leaves your Mac: "+privacyDocURL)))
}

// printReviewPrivacy reports the saved bucket privacy evidence as of the
// prompter's clock, so the review screen agrees with status output.
func printReviewPrivacy(p *prompter, cfg config.Config) {
	report := currentBucketPrivacy(cfg, p.clock())
	switch {
	case report.State == "verified_private":
		p.item(p.style.ok("✓"), "Bucket privacy: native public access blocked at the last check.", nil)
	case report.State == "public_or_risky":
		p.item(p.style.fail("!"), p.style.fail("The bucket looks public ("+privacyReasonText(report.Reason)+")."), []string{"Fix its access before archiving: " + report.GuidanceURL})
	case report.Reason == r2PrivacyUnreadable:
		// Every R2 bucket reads this way, and nothing setup can do changes
		// it, so it is a reminder rather than a warning.
		p.note("Check that public access is disabled for the bucket in the Cloudflare dashboard.")
	default:
		p.warn("Bucket privacy not verified: "+privacyReasonText(report.Reason)+".", "Make sure public access is off: "+report.GuidanceURL)
	}
}

// r2PrivacyUnreadable is the privacy reason of every R2 bucket: its object
// keys cannot read public-access settings (storage.UnknownPrivacy).
const r2PrivacyUnreadable = "r2_management_credentials_not_configured"

func privacyReasonText(reason string) string {
	//lint:ignore LV1001 reason codes come from package storage, and unknown ones are shown as words
	switch reason {
	case r2PrivacyUnreadable:
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
	//lint:ignore LV1001 menu keys are the option keys listed just above
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

// editSetupReview asks which setting to change and asks for it again.
// known, when not nil, lists the projects the apps' history mentions.
func editSetupReview(p *prompter, draft *setupDraft, userHome string, backfilled map[string]bool, known func(config.Config) []backfill.KnownProject) error {
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
	//lint:ignore LV1001 menu keys are the option keys listed just above
	switch choice {
	case "apps":
		if err = chooseHarnesses(p, nil, &draft.Config); err != nil {
			return err
		}
		err = promptStopImported(p, draft)
	case "projects":
		var offered []backfill.KnownProject
		if known != nil {
			offered = known(draft.Config)
		}
		projects, e := promptProjects(p, draft.Config.Archive.Projects, backfilled, offered, userHome)
		if e != nil {
			return e
		}
		draft.Config.Archive.Projects = projects
	case "sessions":
		all, e := p.yesNo("Save sessions even when no skills are used?", !draft.Config.RequireSkillUse)
		if e != nil {
			return e
		}
		draft.Config.RequireSkillUse = !all
	case "retention":
		draft.Config.RetentionDays, err = p.retentionDays(draft.Config.RetentionDays)
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
			terminal.Println(p.out, "Use a relative folder name, such as agent-archive/; do not include .. or a leading slash.")
		}
	case "region":
		draft.Config.Storage.Region, err = promptRegion(p, "Bucket region", draft.Config.Storage.Region)
	}
	return err
}
