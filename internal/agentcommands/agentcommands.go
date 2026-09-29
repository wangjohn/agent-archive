// Package agentcommands is the in-agent commands setup installs: a
// `handoff` skill that runs `agent-archive handoff --to <agent>` from inside
// Claude Code, Codex, or Cursor. It says what each file holds (Files) and
// plans writing and removing them as setup-journal changes (PlanInstall,
// PlanRemoval). Nothing records the files once setup commits, so a file is
// setup's by its content alone (a marker line); anything else at the path
// is left alone.
package agentcommands

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

// File is one command file: where it goes, what it holds, and the
// harnesses (values of config.Config.Harnesses) that read it.
type File struct {
	Harnesses []string
	Path      string
	Content   []byte
}

// allHarnesses is every harness a command file is written for.
var allHarnesses = []string{"codex", "claude", "cursor"}

// Files is the command files for harnesses, running executable. Claude Code
// reads the skills in its configuration directory claudeDir (~/.claude, or
// $CLAUDE_CONFIG_DIR); Codex and Cursor both read ~/.agents/skills (Cursor
// reads ~/.claude/skills too, so the instructions suit any of the three).
func Files(userHome, claudeDir string, harnesses []string, executable string) []File {
	var files []File
	if slices.Contains(harnesses, "claude") {
		files = append(files, File{Harnesses: []string{"claude"}, Path: filepath.Join(claudeDir, "skills", "handoff", "SKILL.md"), Content: claudeSkill(executable)})
	}
	var shared []string
	for _, h := range []string{"codex", "cursor"} {
		if slices.Contains(harnesses, h) {
			shared = append(shared, h)
		}
	}
	if len(shared) > 0 {
		files = append(files, File{Harnesses: shared, Path: filepath.Join(userHome, ".agents", "skills", "handoff", "SKILL.md"), Content: agentsSkill(executable)})
	}
	return files
}

// marker is the line every command file carries. A file with it is setup's,
// whichever release or executable path wrote it; the person keeps a file of
// their own by deleting the line.
const marker = "<!-- Written by agent-archive setup, which replaces this file; agent-archive uninstall removes it. Delete this line to keep your own version. -->"

// claudeSkill is Claude Code's skill: /handoff [agent], with $ARGUMENTS for
// the agent, only the person may invoke it, and it may run the one command
// without asking.
func claudeSkill(executable string) []byte {
	command := shellQuote(executable)
	front := []string{
		"name: handoff",
		"description: Continue this session in another coding agent (Claude Code, Codex, or Cursor) in a new terminal tab.",
		`argument-hint: "[claude|codex|cursor]"`,
		"disable-model-invocation: true",
	}
	// A quoted path would not read back as a plain YAML value or a
	// permission rule; without the rule the command is simply asked about.
	if command == executable {
		front = append(front, "allowed-tools: Bash("+command+" handoff:*)")
	}
	return skill(front, command, "The agent they named, if any: $ARGUMENTS\n\n")
}

// agentsSkill is the skill Codex ($handoff) and Cursor read. Codex has no
// argument substitution, so the agent comes from the request itself.
func agentsSkill(executable string) []byte {
	front := []string{
		"name: handoff",
		"description: Continue this session in another coding agent (Claude Code, Codex, or Cursor) in a new terminal tab. Use only when the person asks to hand off.",
	}
	return skill(front, shellQuote(executable), "")
}

func skill(front []string, command, arguments string) []byte {
	return []byte("---\n" + strings.Join(front, "\n") + "\n---\n" + marker + `

The person wants to continue this session in another coding agent.
` + arguments + `Run exactly this command, and nothing else:

    ` + command + ` handoff --to <agent>

<agent> is the one the person named: claude, codex, or cursor. If they named
none, choose a different agent than yourself: codex if you are Claude Code,
claude if you are Codex or Cursor.

The command opens that agent in a new terminal tab with this session as its
context, and returns at once. Report its output. Do not paste the handoff
content, and do nothing else.
`)
}

// shellQuote quotes s for the shell only when it needs it.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// owned reports whether content is setup's: it carries marker, as what
// Files renders for any executable does.
func owned(content []byte) bool {
	return slices.Contains(strings.Split(string(content), "\n"), marker)
}

// PlanInstall plans the command files for harnesses, running executable:
// each is written where there is none and replaced only while it is setup's
// (see owned), and a file of setup's for a harness no longer chosen is
// removed, as is one in previousClaudeDir, where Claude Code's
// configuration was when setup last ran. Any other file, links and
// directories included, is left alone; a wanted path holding one is
// returned in foreign.
func PlanInstall(userHome, claudeDir string, harnesses []string, executable, previousClaudeDir string) (changes []hooks.Change, foreign []string, err error) {
	wanted := map[string]bool{}
	for _, f := range Files(userHome, claudeDir, harnesses, executable) {
		wanted[f.Path] = true
		current, state, err := read(f.Path)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case state == missing:
			changes = append(changes, hooks.Change{Path: f.Path, After: f.Content, Mode: 0600})
		case state == regular && string(current) == string(f.Content):
		case state == regular && owned(current):
			changes = append(changes, hooks.Change{Path: f.Path, Before: current, After: f.Content, Existed: true, Mode: 0600})
		default:
			foreign = append(foreign, f.Path)
		}
	}
	for _, f := range append(Files(userHome, claudeDir, allHarnesses, ""), Files(userHome, previousClaudeDir, []string{"claude"}, "")...) {
		if wanted[f.Path] {
			continue
		}
		wanted[f.Path] = true // planned once, if both Claude Code paths are one
		change, found, err := planRemoval(f.Path)
		if err != nil {
			return nil, nil, err
		}
		if found {
			changes = append(changes, change)
		}
	}
	return changes, foreign, nil
}

// PlanRemoval plans removing every command file of setup's, for
// uninstall. A file at one of their paths that is not setup's is returned
// in kept and left alone.
func PlanRemoval(userHome, claudeDir string) (changes []hooks.Change, kept []string, err error) {
	for _, f := range Files(userHome, claudeDir, allHarnesses, "") {
		change, found, err := planRemoval(f.Path)
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
func planRemoval(path string) (hooks.Change, bool, error) {
	current, state, err := read(path)
	if err != nil || state != regular || !owned(current) {
		return hooks.Change{}, false, err
	}
	return hooks.Change{Path: path, Before: current, Existed: true, Mode: 0600, Delete: true}, true, nil
}

// Installed is the command files of setup's that are there now, for status.
func Installed(userHome, claudeDir string) []string {
	var paths []string
	for _, f := range Files(userHome, claudeDir, allHarnesses, "") {
		if current, state, err := read(f.Path); err == nil && state == regular && owned(current) {
			paths = append(paths, f.Path)
		}
	}
	return paths
}

// RemoveEmptyDirs removes the directories each command file's path is in,
// deepest first, while they are empty: what writing a file there created,
// once the file is gone. It stops at the home folder, and at Claude Code's
// configuration directory claudeDir; a directory holding anything is kept,
// with every one above it. A link is never removed: os.Remove would unlink
// it whatever it names.
func RemoveEmptyDirs(userHome, claudeDir string) {
	for _, f := range Files(userHome, claudeDir, allHarnesses, "") {
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

type fileState int

const (
	missing fileState = iota
	regular
	other
)

// read is the regular file at path, never followed through a link.
func read(path string) ([]byte, fileState, error) {
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
