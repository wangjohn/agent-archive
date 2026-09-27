package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// preflightCheck is one check setup makes before its first question, so
// that a problem only applying the setup would otherwise find stops it
// while nothing has been asked or changed.
type preflightCheck struct {
	// Label names what was checked, such as "Claude Code hooks".
	Label string
	// Detail is what was found: the file checked, or where the problem is.
	Detail string
	// Problem is what is wrong, when that is said apart from Detail, on a
	// line of its own under it.
	Problem string
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

// print writes one line per check: a ✓, or a ✗ with, under it, the
// problem and the fix. Long lines wrap under their own text.
func (c preflightChecks) print(p *prompter) {
	for _, check := range c {
		line := check.Label + ": " + check.Detail
		if check.OK {
			terminal.Println(p.out, p.style.hang("  "+p.style.okMark()+" ", line))
			continue
		}
		terminal.Println(p.out, p.style.hang("  "+p.style.failMark()+" ", line))
		for _, text := range []string{check.Problem, check.Fix} {
			if text != "" {
				terminal.Println(p.out, p.style.hang("    ", text))
			}
		}
	}
}

// preflightError stops setup when a check before its first question
// failed. yes is whether setup --yes made the checks, which is then the
// command to run again.
type preflightError struct {
	checks preflightChecks
	yes    bool
}

// Error names each failed check again, so a run whose standard output is
// not read, such as a script's setup --yes, still says what blocked it.
func (e *preflightError) Error() string {
	var failed []string
	for _, check := range e.checks {
		if !check.OK {
			failed = append(failed, strings.TrimSuffix(check.Label+": "+check.Detail+": "+check.Problem, ": "))
		}
	}
	return strings.Join(failed, "; ")
}

func (e *preflightError) guidance() string {
	if e.yes {
		return "Nothing was changed. Fix what is marked ✗ above, then run the same agent-archive setup --yes command again."
	}
	return "Nothing was changed, and any unfinished setup is kept. Fix what is marked ✗ above, then run agent-archive setup again."
}

// preflightScope is what setup's checks before its first question cover.
type preflightScope struct {
	// apps are the apps whose hook files are checked.
	apps []string
	// kept are the apps setup --yes --apps cannot leave out now: those
	// installed, or every app while an unfinished setup is saved, since
	// setup --yes refuses to run then. A problem with one of their files
	// is fixed only in the file.
	kept []string
	// r2 is whether the Keychain is checked, for an R2 key.
	r2 bool
	// credentialRef is the saved R2 key's Keychain reference, if any: the
	// item the Keychain check reads.
	credentialRef string
}

// preflight checks what applying the setup needs, before setup asks
// anything: that the hook files of scope's apps (in allHarnesses order)
// are ones setup can install into, that launchctl answers about the
// background job, and, when scope.r2 is set, that the Keychain opens for
// an R2 key.
func preflight(env Env, home, userHome string, scope preflightScope) preflightChecks {
	var checks preflightChecks
	files := hooks.Files{}
	all := env.hookFiles(userHome)
	for _, app := range allHarnesses {
		if containsString(scope.apps, app) {
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
			check.Detail = where
			check.Problem = sentence(problem.Reason)
			check.Fix = "Setup edits only plain JSON. Fix the file, then run agent-archive setup again."
			if !containsString(scope.kept, app) {
				check.Fix += " To set up without " + appName(app) + ", run agent-archive setup --yes with --apps naming the apps you want."
			}
		}
		checks = append(checks, check)
	}

	plist := env.installation(home, userHome).collectorPlist()
	job := preflightCheck{Label: "Background job", Detail: "launchctl responds", OK: true}
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

	if scope.r2 {
		checks = append(checks, keychainCheck(env, scope.credentialRef))
	}
	return checks
}

// keychainProbeRef is the reference the Keychain check reads when no R2
// key is saved yet: no item has it, so a Keychain that opens answers that
// it is missing.
const keychainProbeRef = "agent-archive-setup-check"

// keychainCheck checks that the Keychain, where setup keeps an R2 key,
// opens: that this build can use it, and that reading ref (or a probe
// reference) without showing UI is not refused as locked or unavailable.
// A missing or unreadable item is no problem here: setup asks for the key
// again.
func keychainCheck(env Env, ref string) preflightCheck {
	kc, err := env.keychain()
	if err == nil {
		if ref == "" {
			ref = keychainProbeRef
		}
		if _, err = kc.Load(context.Background(), ref); !errors.Is(err, credentials.ErrUnavailable) {
			err = nil
		}
	}
	if err != nil {
		fix := "Use the release build of agent-archive, which can open the Keychain, or store in Amazon S3 with agent-archive setup --yes --provider s3."
		if errors.Is(err, credentials.ErrKeychainLocked) {
			fix = "Unlock the login Keychain (log in, or open Keychain Access), then run agent-archive setup again, or store in Amazon S3 with agent-archive setup --yes --provider s3."
		}
		return preflightCheck{
			Label:  "Keychain",
			Detail: "cannot be opened, so an R2 key cannot be kept (" + strings.TrimSuffix(err.Error(), ".") + ")",
			Fix:    fix,
		}
	}
	return preflightCheck{Label: "Keychain", Detail: "opens (for the R2 key)", OK: true}
}

// preflightApps are the apps whose hook files interactive setup checks
// before its first question: every app detected on this Mac that neither
// the saved configuration nor the unfinished setup leaves out, and every
// app the saved configuration or the unfinished setup includes.
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

// sentence makes a lower-case reason a sentence of its own: it starts with
// a capital letter and ends with one period.
func sentence(text string) string {
	text = strings.TrimSuffix(strings.TrimSpace(text), ".")
	if text == "" {
		return ""
	}
	r, size := utf8.DecodeRuneInString(text)
	return string(unicode.ToUpper(r)) + text[size:] + "."
}
