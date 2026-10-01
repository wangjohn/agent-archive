package hooks

import (
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/fileapply"
	"github.com/wangjohn/agent-archive/internal/filechange"
	"github.com/wangjohn/agent-archive/internal/jsonedit"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Change is the shared value-only file plan.
type Change = filechange.Change

// Files maps each harness to the absolute path of its hook configuration
// file. See ResolveFiles.
type Files map[string]string

// ResolveFiles finds each application's hook configuration file the way the
// application itself does: Claude Code reads $CLAUDE_CONFIG_DIR/settings.json
// in place of ~/.claude/settings.json, and Codex reads $CODEX_HOME/hooks.json
// in place of ~/.codex/hooks.json. Cursor has no such variable. lookupEnv
// reads the environment setup runs in, which is the one the user starts the
// applications from; a relative directory is taken relative to the current
// directory, as the application would.
func ResolveFiles(userHome string, lookupEnv func(string) (string, bool), ports agentapi.HooksLookup) Files {
	cwd, _ := os.Getwd()
	env := map[string]string{}
	for _, name := range ports.HookAgents() {
		p, ok := ports.LookupHooks(name)
		if !ok {
			continue
		}
		for _, key := range p.EnvironmentKeys() {
			if v, found := lookupEnv(key); found {
				env[key] = v
			}
		}
	}
	files := Files{}
	for _, name := range ports.HookAgents() {
		p, ok := ports.LookupHooks(name)
		if ok {
			files[name] = p.Location(agentapi.HookLocations{UserHome: userHome, WorkingDirectory: cwd, Environment: env})
		}
	}
	return files
}

func (f Files) path(harness string) (string, error) {

	path := f[harness]
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("no hook configuration file for %s", harness)
	}
	return path, nil
}

// Plan prepares installing hook for each harness into its file in files.
func Plan(files Files, hook Hook, harnesses []string) ([]Change, error) {
	changes := []Change{}
	for _, h := range harnesses {
		change, err := planFile(files, h, hook)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	return changes, nil
}

// readError is Plan's error for a hook file that is there but cannot be
// read. Its message names only the path; the cause is kept for Validate.
type readError struct {
	path  string
	cause error
}

func (e *readError) Error() string { return "cannot read " + e.path }

func (e *readError) Unwrap() error { return e.cause }

// planFile prepares installing hook for harness into its file in files. It
// is the one place Plan and Validate check a file, so the two cannot
// disagree about which files setup can install into.
func planFile(files Files, harness string, hook Hook) (Change, error) {
	file, port, owner, err := observe(files, hook, harness)
	if err != nil {
		return Change{}, err
	}
	if file.ReadError != nil {
		return Change{}, &readError{path: file.Path, cause: file.ReadError}
	}
	changes, err := port.Plan(agentapi.HookPlanRequest{Action: agentapi.HookInstall, File: file, Owner: owner})
	if err != nil {
		return Change{}, fmt.Errorf("%s: %w", file.Path, err)
	}
	if len(changes) != 1 {
		return Change{}, errors.New("hook install must plan one file")
	}
	return changes[0], nil
}

// Problem is why setup cannot install into one hook file, as Validate finds
// it.
type Problem struct {
	// Harness is the application the file belongs to, as files names it.
	Harness string
	// Path is the file as files names it: a symbolic link is reported by
	// its own path, not the file it points to.
	Path string
	// Line is the 1-based line of the file the problem is on. It is 0 when
	// the problem is not at one place, such as a duplicate key under
	// "hooks", which Reason names by its path in the JSON instead.
	Line int
	// Column is the 1-based column on Line, counted in bytes, or 0 with
	// Line.
	Column int
	// Reason is what is wrong and, for the usual mistakes, how to fix it,
	// without the path, line and column.
	Reason string
	// Err is the error Plan fails with for this file, word for word.
	Err error
}

// validationHook stands in for the Hook setup installs when Validate runs
// Merge: a file Merge refuses is refused for what it holds, never for the
// command installed into it, so any valid Hook refuses the same files.

// Validate checks every hook file in files the way Plan does before it
// installs into one, and returns a Problem for each file Plan would refuse:
// one that is not plain JSON (a comment, a trailing comma, a byte-order
// mark), holds a duplicate key setup would have to choose between, or has
// hooks of a shape setup cannot edit. It reads the files, through any
// symbolic links, and changes nothing. A missing file is no problem: setup
// creates it. Problems come in harness order.
func Validate(files Files, ports agentapi.HooksLookup) []Problem {
	validationHook := Hook{Executable: string(filepath.Separator), Ports: ports}
	var problems []Problem
	for _, harness := range slices.Sorted(maps.Keys(files)) {
		_, err := planFile(files, harness, validationHook)
		if err == nil {
			continue
		}
		path := files[harness]
		p := Problem{Harness: harness, Path: path, Err: err}
		p.Line, p.Column, p.Reason = jsonedit.Problem(err)
		if p.Reason == "" {
			p.Reason = strings.TrimPrefix(strings.TrimPrefix(err.Error(), path+": "), jsonedit.ErrInvalidConfiguration.Error()+": ")
			var unread *readError
			if errors.As(err, &unread) {
				p.Reason = "the file cannot be read"
				if cause := errors.Unwrap(unread.cause); cause != nil {
					p.Reason += ": " + cause.Error()
				}
			}
		}
		problems = append(problems, p)
	}
	return problems
}

// PlanRemovalOf prepares the inverse of Plan for one harness, for uninstall:
// a Change whose After is the current file with only hook's installation's
// handlers (and the prototype's) stripped (see Remove). found is false, and
// there is no Change, when the harness's hook file is missing or holds
// nothing of hook's installation, so an unrelated configuration, or one only
// another installation's hooks are in, is never rewritten or reformatted.
// Apply the result with Apply, which keeps its refuse-on-concurrent-edit and
// rollback behavior.
func PlanRemovalOf(files Files, hook Hook, harness string) (change Change, found bool, err error) {
	file, port, owner, err := observe(files, hook, harness)
	if err != nil {
		return Change{}, false, err
	}
	changes, err := port.Plan(agentapi.HookPlanRequest{Action: agentapi.HookRemove, File: file, Owner: owner})
	if err != nil {
		return Change{}, false, fmt.Errorf("%s: %w", file.Path, err)
	}
	if len(changes) == 0 {
		return Change{}, false, nil
	}
	if len(changes) != 1 {
		return Change{}, false, errors.New("hook remove must plan one file")
	}
	return changes[0], true, nil
}

// Installed reports whether harness's hook file holds exactly what setup
// installs: in every lifecycle event, exactly one handler of hook's
// installation running hook's command, wherever it sits among the user's own
// handlers, and no handler of that installation (or the prototype) anywhere
// else. Formatting, key order, the user's own handlers, and another
// installation's do not affect the result (see OtherInstallations).
func Installed(files Files, hook Hook, harness string) (bool, error) {
	result, err := Inspect(files, hook, harness)
	return result.Installed, err
}

// OtherInstallation is local information about another managed owner.
type OtherInstallation = agentapi.HookOtherOwner

// OtherInstallations reports foreign handlers without changing settings.
func OtherInstallations(files Files, hook Hook, harness string) ([]OtherInstallation, error) {
	result, err := Inspect(files, hook, harness)
	return result.Others, err
}

// Inspect supplies bounded host observations to the integration's pure inspector.
func Inspect(files Files, hook Hook, harness string) (agentapi.HookInspection, error) {
	file, port, owner, err := observe(files, hook, harness)
	if err != nil {
		return agentapi.HookInspection{State: agentapi.HookUnreadable, Reason: "settings_unreadable"}, err
	}
	return port.Inspect(agentapi.HookInspectionRequest{File: file, Owner: owner})
}

// ErrChanged reports a concurrent edit after planning.
var ErrChanged = fileapply.ErrChanged

// Applied reports whether a file retains the planned result.
func Applied(c Change) bool { return fileapply.Applied(c) }

// Unapplied reports whether a file retains the original observation.
func Unapplied(c Change) bool { return fileapply.Unapplied(c) }

// Apply applies generic byte plans with rollback on failure.
func Apply(c []Change) error { return fileapply.Apply(c) }

// Rollback restores generic plans in reverse order.
func Rollback(c []Change) error { return fileapply.Rollback(c) }
func apply(c []Change, target func(string) (string, error)) error {
	return fileapply.ApplyWithTarget(c, target)
}
func resolveTarget(path string) (string, error) { return fileapply.ResolveTarget(path) }
