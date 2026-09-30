// Package agentskills is the agent skills setup installs into Claude Code,
// Codex, and Cursor: the Registry of skills, each rendered per destination
// (a Skill), such as `handoff`, which runs `agent-archive handoff --to
// <agent>` from inside an agent. It says what each file holds (Files) and
// plans writing and removing them as setup-journal changes (PlanInstall,
// PlanRemoval). Nothing records the files once setup commits, so a file is
// setup's by its content alone (a marker line), and an installation's by
// the data directory its command names; anything else at the path is left
// alone. Stale finds the files an upgrade has outdated. The plan is
// dev/specs/agent-skill.md.
package agentskills

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
)

// File is one skill file: which skill it is (Skill.Name), where it goes,
// what it holds, and the harnesses (values of config.Config.Harnesses) that
// read it.
type File struct {
	Skill     string
	Harnesses []string
	Path      string
	Content   []byte
}

// allHarnesses is every harness a skill file is written for.
var allHarnesses = []string{"codex", "claude", "cursor"}

// Files is the skill files of every skill in Registry for harnesses, in
// Registry order (each skill's Claude Code file, then its shared one),
// running executable with AGENT_ARCHIVE_HOME=dataHome, or with none for the
// default data directory (dataHome ""), as the installation's hooks do: the
// agent's environment need not have it. Claude Code reads the skills in its
// configuration directory claudeDir (~/.claude, or $CLAUDE_CONFIG_DIR);
// Codex and Cursor both read ~/.agents/skills (Cursor reads ~/.claude/skills
// too, so the instructions suit any of the three).
func Files(userHome, claudeDir string, harnesses []string, executable, dataHome string) []File {
	return skillFiles(Registry, userHome, claudeDir, harnesses, executable, dataHome)
}

// skillFiles is Files for the skills in registry.
func skillFiles(registry []Skill, userHome, claudeDir string, harnesses []string, executable, dataHome string) []File {
	var files []File
	var shared []string
	for _, h := range []string{"codex", "cursor"} {
		if slices.Contains(harnesses, h) {
			shared = append(shared, h)
		}
	}
	for _, s := range registry {
		if slices.Contains(harnesses, "claude") {
			files = append(files, File{Skill: s.Name, Harnesses: []string{"claude"}, Path: filepath.Join(claudeDir, "skills", s.Name, "SKILL.md"), Content: s.Render(Claude, executable, dataHome)})
		}
		if len(shared) > 0 {
			files = append(files, File{Skill: s.Name, Harnesses: shared, Path: filepath.Join(userHome, ".agents", "skills", s.Name, "SKILL.md"), Content: s.Render(Shared, executable, dataHome)})
		}
	}
	return files
}

// marker is the line every skill file carries. A file with it is setup's,
// whichever release or executable path wrote it; the person keeps a file of
// their own by deleting the line.
const marker = "<!-- Written by agent-archive setup, which replaces this file; agent-archive uninstall removes it. Delete this line to keep your own version. -->"

// dataHomeVariable begins a command line naming a relocated data directory.
const dataHomeVariable = "AGENT_ARCHIVE_HOME="

// commandLine is the command a skill runs, less its arguments.
func commandLine(executable, dataHome string) string {
	if dataHome == "" {
		return shellQuote(executable)
	}
	return dataHomeVariable + shellQuote(dataHome) + " " + shellQuote(executable)
}

// shellQuote quotes s for the shell only when it needs it.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// owned reports whether content is setup's for the installation in
// dataHome: it carries marker, as what Files renders for any executable
// does, and its command names dataHome, or no data directory for the
// default installation: another installation sharing this HOME, with hook
// files of its own, keeps its file.
func owned(content []byte, dataHome string) bool {
	if !slices.Contains(strings.Split(string(content), "\n"), marker) {
		return false
	}
	if dataHome == "" {
		return !strings.Contains(string(content), dataHomeVariable)
	}
	return strings.Contains(string(content), dataHomeVariable+shellQuote(dataHome)+" ")
}

// PlanInstall plans the skill files for harnesses, running executable:
// each is written where there is none and replaced only while it is setup's
// (see owned), and a file of setup's for a harness no longer chosen is
// removed, as is one in previousClaudeDir, where Claude Code's
// configuration was when setup last ran. Any other file, links and
// directories included, is left alone; a wanted path holding one is
// returned in foreign.
func PlanInstall(userHome, claudeDir string, harnesses []string, executable, dataHome, previousClaudeDir string) (changes []hooks.Change, foreign []string, err error) {
	return planInstall(Registry, userHome, claudeDir, harnesses, executable, dataHome, previousClaudeDir)
}

// planInstall is PlanInstall for the skills in registry.
func planInstall(registry []Skill, userHome, claudeDir string, harnesses []string, executable, dataHome, previousClaudeDir string) (changes []hooks.Change, foreign []string, err error) {
	wanted := map[string]bool{}
	for _, f := range skillFiles(registry, userHome, claudeDir, harnesses, executable, dataHome) {
		wanted[f.Path] = true
		current, state, err := read(f.Path)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case state == missing:
			changes = append(changes, hooks.Change{Path: f.Path, After: f.Content, Mode: 0600})
		case state == regular && string(current) == string(f.Content):
		case state == regular && owned(current, dataHome):
			changes = append(changes, hooks.Change{Path: f.Path, Before: current, After: f.Content, Existed: true, Mode: mode(f.Path)})
		default:
			foreign = append(foreign, f.Path)
		}
	}
	for _, f := range append(skillFiles(registry, userHome, claudeDir, allHarnesses, "", ""), skillFiles(registry, userHome, previousClaudeDir, []string{"claude"}, "", "")...) {
		if wanted[f.Path] {
			continue
		}
		wanted[f.Path] = true // planned once, if both Claude Code paths are one
		change, found, err := planRemoval(f.Path, dataHome)
		if err != nil {
			return nil, nil, err
		}
		if found {
			changes = append(changes, change)
		}
	}
	return changes, foreign, nil
}

// PlanRemoval plans removing every skill file of setup's, for uninstall. A
// file at one of their paths that is not setup's is returned in kept and
// left alone.
func PlanRemoval(userHome, claudeDir, dataHome string) (changes []hooks.Change, kept []string, err error) {
	return planRemovalOf(Registry, userHome, claudeDir, dataHome)
}

// planRemovalOf is PlanRemoval for the skills in registry.
func planRemovalOf(registry []Skill, userHome, claudeDir, dataHome string) (changes []hooks.Change, kept []string, err error) {
	for _, f := range skillFiles(registry, userHome, claudeDir, allHarnesses, "", "") {
		change, found, err := planRemoval(f.Path, dataHome)
		if err != nil {
			return nil, nil, err
		}
		if found {
			changes = append(changes, change)
		} else if _, state, _ := read(f.Path); state != missing {
			kept = append(kept, f.Path)
		}
	}
	return changes, kept, nil
}

// planRemoval is the Change deleting path, when it is setup's.
func planRemoval(path, dataHome string) (hooks.Change, bool, error) {
	current, state, err := read(path)
	if err != nil || state != regular || !owned(current, dataHome) {
		return hooks.Change{}, false, err
	}
	return hooks.Change{Path: path, Before: current, Existed: true, Mode: mode(path), Delete: true}, true, nil
}

// Installed is the skill files of setup's that are there now, for status.
func Installed(userHome, claudeDir, dataHome string) []string {
	return installedOf(Registry, userHome, claudeDir, dataHome)
}

// installedOf is Installed for the skills in registry.
func installedOf(registry []Skill, userHome, claudeDir, dataHome string) []string {
	var paths []string
	for _, f := range skillFiles(registry, userHome, claudeDir, allHarnesses, "", "") {
		if current, state, err := read(f.Path); err == nil && state == regular && owned(current, dataHome) {
			paths = append(paths, f.Path)
		}
	}
	return paths
}

// Stale is the skill files of setup's that are there but differ from what
// this release renders for executable: written by an earlier release or
// executable, so setup (which replaces a file it owns) refreshes them. It
// is a subset of Installed, for status. With no executable recorded there
// is nothing to compare with, and it reports none.
func Stale(userHome, claudeDir, executable, dataHome string) []string {
	return staleOf(Registry, userHome, claudeDir, executable, dataHome)
}

// staleOf is Stale for the skills in registry.
func staleOf(registry []Skill, userHome, claudeDir, executable, dataHome string) []string {
	if executable == "" {
		return nil
	}
	var paths []string
	for _, f := range skillFiles(registry, userHome, claudeDir, allHarnesses, executable, dataHome) {
		if current, state, err := read(f.Path); err == nil && state == regular && owned(current, dataHome) && string(current) != string(f.Content) {
			paths = append(paths, f.Path)
		}
	}
	return paths
}

// RemoveEmptyDirs removes the directories each skill file's path is in,
// deepest first, while they are empty: what writing a file there created,
// once the file is gone. It stops at the home folder, and at Claude Code's
// configuration directory claudeDir; a directory holding anything is kept,
// with every one above it. A link is never removed: os.Remove would unlink
// it whatever it names.
func RemoveEmptyDirs(userHome, claudeDir string) {
	removeEmptyDirs(Registry, userHome, claudeDir)
}

// removeEmptyDirs is RemoveEmptyDirs for the skills in registry.
func removeEmptyDirs(registry []Skill, userHome, claudeDir string) {
	for _, f := range skillFiles(registry, userHome, claudeDir, allHarnesses, "", "") {
		stop := userHome
		if slices.Contains(f.Harnesses, "claude") {
			stop = claudeDir
		}
		for dir := filepath.Dir(f.Path); dir != filepath.Clean(stop) && local.PathWithin(dir, stop); dir = filepath.Dir(dir) {
			if info, err := os.Lstat(dir); err != nil || !info.IsDir() || os.Remove(dir) != nil {
				break
			}
		}
	}
}

// mode is the permissions of the file at path, for a Change replacing or
// removing it, so that a rollback puts them back with its content.
func mode(path string) os.FileMode {
	if info, err := os.Lstat(path); err == nil {
		return info.Mode().Perm()
	}
	return 0600
}

type fileState int

const (
	missing fileState = iota
	regular
	other
)

// read is the regular file at path, never followed through a link: not
// the file's own, nor the skill's directory's (a skill of the person's own
// linked in, which a write would land in). A linked skills directory above
// it is followed, as hook files are through a dotfile manager's links.
func read(path string) ([]byte, fileState, error) {
	if info, err := os.Lstat(filepath.Dir(path)); err == nil && !info.IsDir() {
		return nil, other, nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, missing, nil
	}
	if err != nil {
		return nil, other, err
	}
	if !info.Mode().IsRegular() {
		return nil, other, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, other, err
	}
	return data, regular, nil
}
