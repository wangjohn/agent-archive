package cli

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
)

// defaultDataHome is the data directory used when AGENT_ARCHIVE_HOME is
// unset (see local.Home), with its symlinks resolved the same way.
func defaultDataHome(userHome string) string {
	return canonicalPath(filepath.Join(userHome, ".local", "share", "agent-archive"))
}

// canonicalPath is path with its existing symlinks resolved, as local.Home
// resolves the data directory, so two spellings of one directory compare
// equal.
func canonicalPath(path string) string {
	if resolved, err := local.ResolveExistingSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// isDefaultDataHome reports whether home is the data directory a process
// finds without AGENT_ARCHIVE_HOME.
func isDefaultDataHome(home, userHome string) bool {
	return canonicalPath(home) == defaultDataHome(userHome)
}

// installedHook is what setup installs into the apps' hook files for the
// data directory home: a non-default directory travels in the command.
func installedHook(home, userHome, executable string) hooks.Hook {
	hook := hooks.Hook{Executable: executable}
	if !isDefaultDataHome(home, userHome) {
		hook.DataHome = home
	}
	return hook
}

// collectorPlist is the LaunchAgent path of home's background collector;
// its file name is its label (see hooks.CollectorLabel).
func collectorPlist(home, userHome string) string {
	return filepath.Join(userHome, "Library", "LaunchAgents", hooks.CollectorLabel(canonicalPath(home), defaultDataHome(userHome))+".plist")
}

// launchLabel is the launchd label of the job a plist defines. Every job
// this tool loads is named after its label, so the file name is the label.
func launchLabel(plist string) string {
	return strings.TrimSuffix(filepath.Base(plist), ".plist")
}

// previousCollectorPlist is the LaunchAgent an earlier release installed
// under the default label for a non-default data directory, before labels
// were derived from the directory: "" unless that plist exists and runs the
// collector for home. A plist for any other directory is never returned, so
// the default installation is never touched from a secondary one.
func previousCollectorPlist(home, userHome string) string {
	if isDefaultDataHome(home, userHome) {
		return ""
	}
	path := filepath.Join(userHome, "Library", "LaunchAgents", hooks.LaunchLabel+".plist")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	dataHome, err := hooks.LaunchAgentDataHome(data)
	if err != nil || dataHome == "" {
		return ""
	}
	if canonicalPath(dataHome) != canonicalPath(home) {
		return ""
	}
	return path
}

// installedCollectorPlist is the LaunchAgent status reports on: the one for
// home's own label, or else one an earlier release installed for home under
// the default label.
func installedCollectorPlist(home, userHome string) string {
	current := collectorPlist(home, userHome)
	if _, err := os.Stat(current); err != nil {
		if previous := previousCollectorPlist(home, userHome); previous != "" {
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

// installedHookFiles is where setup installed each app's hooks: the paths it
// recorded, so status and uninstall find them from a shell without the
// variables setup saw, and the current environment's paths for apps a
// configuration from before the record has no entry for.
func (e Env) installedHookFiles(userHome string, cfg config.Config) hooks.Files {
	files := e.hookFiles(userHome)
	for app, path := range cfg.HookFiles {
		if filepath.IsAbs(path) {
			files[app] = path
		}
	}
	return files
}
