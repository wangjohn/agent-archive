package cli

import (
	"fmt"
	"strings"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

// preflightCheck is one check setup makes before its first question, so
// that a problem only applying the setup would otherwise find stops it
// while nothing has been asked or changed.
type preflightCheck struct {
	// Label names what was checked, such as "Claude Code hooks".
	Label string
	// Detail is what was found: the file checked, or where and what the
	// problem is.
	Detail string
	// Fix is what to do about a problem; empty when OK.
	Fix string
	// OK is whether the check passed. A check that did not pass blocks
	// setup.
	OK bool
}

// preflightChecks are setup's checks before its first question, in the
// order they ran. The review keeps them to show as a checklist.
type preflightChecks []preflightCheck

// blocked reports whether any check failed.
func (c preflightChecks) blocked() bool {
	for _, check := range c {
		if !check.OK {
			return true
		}
	}
	return false
}

// print writes one line per check: a ✓, or a ✗ with the problem and, under
// it, the fix.
func (c preflightChecks) print(p *prompter) {
	for _, check := range c {
		line := check.Label + ": " + check.Detail
		if check.OK {
			p.item(p.style.okMark(), line, nil)
			continue
		}
		p.item(p.style.failMark(), line, []string{check.Fix})
	}
}

// preflightError stops setup when a check before its first question
// failed.
type preflightError struct{ checks preflightChecks }

// Error names each failed check again, so a run whose standard output is
// not read, such as a script's setup --yes, still says what blocked it.
func (e *preflightError) Error() string {
	var failed []string
	for _, check := range e.checks {
		if !check.OK {
			failed = append(failed, check.Label+": "+check.Detail)
		}
	}
	return strings.Join(failed, "; ")
}

func (e *preflightError) guidance() string {
	return "Nothing was changed, and any unfinished setup is kept. Fix what is marked ✗ above, then run agent-archive setup again."
}

// preflight checks what applying the setup needs, before setup asks
// anything: that the hook files of apps (in allHarnesses order) are ones
// setup can install into, that launchctl answers about the background
// job, and, when keychain is set, that the Keychain opens for an R2 key.
func preflight(env Env, home, userHome string, apps []string, keychain bool) preflightChecks {
	var checks preflightChecks
	files := hooks.Files{}
	all := env.hookFiles(userHome)
	for _, app := range allHarnesses {
		if containsString(apps, app) {
			files[app] = all[app]
		}
	}
	problems := map[string]hooks.Problem{}
	for _, problem := range hooks.Validate(files) {
		problems[problem.Harness] = problem
	}
	for _, app := range allHarnesses {
		path, ok := files[app]
		if !ok {
			continue
		}
		check := preflightCheck{Label: appName(app) + " hooks", Detail: displayPath(path, userHome), OK: true}
		if problem, found := problems[app]; found {
			where := check.Detail
			if problem.Line > 0 {
				where = fmt.Sprintf("%s:%d:%d", where, problem.Line, problem.Column)
			}
			check.OK = false
			check.Detail = where + ": " + problem.Reason
			check.Fix = "Fix the file (setup edits only plain JSON), then run agent-archive setup again. To set up without " + appName(app) + ", run agent-archive setup --yes with --apps naming the apps you want."
		}
		checks = append(checks, check)
	}

	plist := env.installation(home, userHome).collectorPlist()
	job := preflightCheck{Label: "Background job", Detail: "launchctl answers", OK: true}
	switch env.jobState(plist) {
	case "unknown":
		job.OK = false
		job.Detail = "launchctl did not say whether the " + launchLabel(plist) + " job is loaded, and setup loads it only when it can tell"
		job.Fix = "Check that launchctl print gui/$(id -u) works in Terminal, then run agent-archive setup again."
	case setupjournal.JobAnotherInstallation:
		job.OK = false
		job.Detail = fmt.Sprintf("launchd's %s job was loaded from a plist other than %s, so it belongs to another installation", launchLabel(plist), displayPath(plist, userHome))
		job.Fix = "Uninstall that installation first, or set AGENT_ARCHIVE_HOME to a directory of this installation's own."
	}
	checks = append(checks, job)

	if keychain {
		checks = append(checks, keychainCheck(env))
	}
	return checks
}

// keychainCheck checks that the Keychain, where setup keeps an R2 key,
// opens.
func keychainCheck(env Env) preflightCheck {
	if _, err := env.keychain(); err != nil {
		return preflightCheck{
			Label:  "Keychain",
			Detail: "cannot be opened, so an R2 key cannot be kept (" + strings.TrimSuffix(err.Error(), ".") + ")",
			Fix:    "Use the release build of agent-archive, which can open the Keychain, or store in Amazon S3 with agent-archive setup --yes --provider s3.",
		}
	}
	return preflightCheck{Label: "Keychain", Detail: "opens, for the R2 key", OK: true}
}

// preflightApps are the apps whose hook files interactive setup checks
// before its first question: every app detected on this Mac that the saved
// configuration does not leave out, and every app the saved configuration
// or the unfinished setup includes.
func preflightApps(detected, saved, declined, draft []string) []string {
	var apps []string
	for _, app := range allHarnesses {
		switch {
		case containsString(saved, app), containsString(draft, app):
		case containsString(detected, app) && !containsString(declined, app):
		default:
			continue
		}
		apps = append(apps, app)
	}
	return apps
}
