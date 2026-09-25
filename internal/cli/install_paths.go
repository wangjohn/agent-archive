package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
)

// installation locates one data directory's integrations: the hook command
// its hooks run and its background collector's LaunchAgent.
type installation struct {
	home     string // the data directory
	userHome string // $HOME, where LaunchAgents and app configs live
	// accountHome is the account's own home directory from the user
	// database, which a sandbox that only overrides $HOME does not change.
	accountHome string
}

func (e Env) installation(home, userHome string) installation {
	return installation{home: home, userHome: userHome, accountHome: e.accountHome()}
}

// defaultDataHome is the data directory of the account's own default
// installation: ~/.local/share/agent-archive under the account's real home,
// spelled canonically (local.CanonicalPath).
func (in installation) defaultDataHome() string {
	return local.CanonicalPath(filepath.Join(in.accountHome, ".local", "share", "agent-archive"))
}

// isDefault reports whether this is the account's default installation,
// however its directory is spelled (a symlink, or another case on a
// case-insensitive volume): the same test hooks use to tell installations
// apart (local.SameLocation). Anything else (AGENT_ARCHIVE_HOME set
// elsewhere, or a sandbox that overrides $HOME and so moves the data
// directory with it) is not, and gets labels and hook commands of its own.
func (in installation) isDefault() bool {
	return in.accountHome != "" && local.SameLocation(in.home, in.defaultDataHome())
}

// hook is what setup installs into the apps' hook files: a non-default data
// directory travels in the command. It also identifies this installation's
// handlers among other installations' in the same files (see hooks.Hook).
func (in installation) hook(executable string) hooks.Hook {
	dataHome, defaultHome := "", ""
	if !in.isDefault() {
		dataHome = in.home
	}
	if in.accountHome != "" {
		defaultHome = in.defaultDataHome()
	}
	return hooks.Hook{Executable: executable, DataHome: dataHome, DefaultDataHome: defaultHome}
}

// owner identifies this installation's hook handlers, for removing them;
// the executable they run does not matter there.
func (in installation) owner() hooks.Hook { return in.hook("") }

// otherInstallationProblems describes, one message per file, the hook
// handlers another installation (another data directory) put in the hook
// files of apps: setup will not install beside them, and uninstall and
// status leave them alone. A file that cannot be read is skipped; setup and
// status report that on their own.
func (in installation) otherInstallationProblems(files hooks.Files, apps []string) []string {
	var problems []string
	for _, app := range apps {
		others, err := hooks.OtherInstallations(files, in.owner(), app)
		if err != nil || len(others) == 0 {
			continue
		}
		problems = append(problems, describeOtherInstallations(files[app], app, others))
	}
	return problems
}

// describeOtherInstallations says whose handlers are in path and how to
// resolve it: every command that finds another installation's hooks
// describes them this way.
func describeOtherInstallations(path, app string, others []hooks.OtherInstallation) string {
	var owners, fixes []string
	for _, other := range others {
		switch {
		case other.Command != "":
			owners = append(owners, fmt.Sprintf("an edited agent-archive hook whose data directory cannot be read (%s)", other.Command))
			fixes = append(fixes, "remove that hook from the file by hand")
		case other.Default:
			owner := "the default installation"
			if other.DataHome != "" {
				owner += " in " + other.DataHome
			}
			owners = append(owners, owner)
			fixes = append(fixes, "run agent-archive uninstall with AGENT_ARCHIVE_HOME unset")
		default:
			owners = append(owners, "the installation in "+other.DataHome)
			fixes = append(fixes, fmt.Sprintf("run AGENT_ARCHIVE_HOME=%s agent-archive uninstall", shellQuote(other.DataHome)))
		}
	}
	return fmt.Sprintf("%s holds %s hooks of another agent-archive installation (%s), which this installation never changes. Setup installs no %s hooks beside them, since two installations would each capture every session. To remove them, %s. To test an installation on its own, give it its own HOME (or CLAUDE_CONFIG_DIR and CODEX_HOME), so the apps' hook files are separate too.",
		path, appName(app), strings.Join(owners, "; "), appName(app), strings.Join(fixes, "; "))
}

// shellQuote quotes s for a shell command line the user copies, only when it
// needs quoting.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+~") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// label is the background collector's launchd label (see
// hooks.CollectorLabel): the default label only for the default
// installation.
func (in installation) label() string {
	if in.isDefault() {
		return hooks.LaunchLabel
	}
	return hooks.CollectorLabel(local.CanonicalPath(in.home), "")
}

// collectorPlist is the LaunchAgent path of the background collector; its
// file name is its label.
func (in installation) collectorPlist() string {
	return filepath.Join(in.userHome, "Library", "LaunchAgents", in.label()+".plist")
}

// launchLabel is the launchd label of the job a plist defines. Every job
// this tool loads is named after its label, so the file name is the label.
func launchLabel(plist string) string {
	return strings.TrimSuffix(filepath.Base(plist), ".plist")
}

// previousCollectorPlist is the LaunchAgent an earlier release installed for
// this data directory under a label other than its own: "" unless that
// plist exists and runs the collector for this data directory. There are
// two such labels. The default one, which releases before labels were
// derived from the directory gave a non-default data directory. And the
// label derived from the directory as spelled (its symlinks resolved but
// not its case), which releases before CanonicalPath gave a directory
// spelled in another case than it is listed in, the default directory
// included. A plist for any other directory is never returned, so another
// installation's is never touched.
func (in installation) previousCollectorPlist() string {
	var labels []string
	if !in.isDefault() {
		labels = append(labels, hooks.LaunchLabel)
	}
	spelled := filepath.Clean(in.home)
	if resolved, err := local.ResolveExistingSymlinks(in.home); err == nil {
		spelled = resolved
	}
	if label := hooks.CollectorLabel(spelled, ""); label != in.label() {
		labels = append(labels, label)
	}
	for _, label := range labels {
		path := filepath.Join(in.userHome, "Library", "LaunchAgents", label+".plist")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		dataHome, err := hooks.LaunchAgentDataHome(data)
		if err != nil || dataHome == "" || !local.SameLocation(dataHome, in.home) {
			continue
		}
		return path
	}
	return ""
}

// installedCollectorPlist is the LaunchAgent status reports on: the one for
// this installation's own label, or else one an earlier release installed
// for it under the default label.
func (in installation) installedCollectorPlist() string {
	current := in.collectorPlist()
	if _, err := os.Stat(current); err != nil {
		if previous := in.previousCollectorPlist(); previous != "" {
			return previous
		}
	}
	return current
}

// hookFiles resolves each app's hook file from the environment this command
// runs in (CLAUDE_CONFIG_DIR, CODEX_HOME); see hooks.ResolveFiles.
func (e Env) hookFiles(userHome string) hooks.Files {
	return hooks.ResolveFiles(userHome, e.lookupEnv)
}

// legacyHookFiles are the fixed paths every release before hook_files
// installed into, whatever CLAUDE_CONFIG_DIR or CODEX_HOME said.
func legacyHookFiles(userHome string) hooks.Files {
	return hooks.ResolveFiles(userHome, func(string) (string, bool) { return "", false })
}

// installedHookFiles is where setup installed each app's hooks: the paths it
// recorded, so status and uninstall find them from a shell without the
// variables setup saw. An app a configuration from before the record has no
// entry for was installed at its legacy path, never where the current
// environment points.
func (e Env) installedHookFiles(userHome string, cfg config.Config) hooks.Files {
	files := legacyHookFiles(userHome)
	for app, path := range cfg.HookFiles {
		if filepath.IsAbs(path) {
			files[app] = path
		}
	}
	return files
}
