package hooks

import (
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/hookconfig"
	"github.com/wangjohn/agent-archive/internal/filechange"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Change is the shared value-only file plan.
type Change = filechange.Change

// Applied reports whether the file at c.Path is as c leaves it: deleted, or
// holding After.
func Applied(c Change) bool {
	current, err := os.ReadFile(c.Path)
	if c.Delete {
		return os.IsNotExist(err)
	}
	return err == nil && string(current) == string(c.After)
}

// Unapplied reports whether the file at c.Path is as c found it: holding
// Before, or absent when it did not exist.
func Unapplied(c Change) bool {
	current, err := os.ReadFile(c.Path)
	if !c.Existed {
		return os.IsNotExist(err)
	}
	return err == nil && string(current) == string(c.Before)
}

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
		p.Line, p.Column, p.Reason = hookconfig.Problem(err)
		if p.Reason == "" {
			p.Reason = strings.TrimPrefix(err.Error(), path+": ")
		}
		problems = append(problems, p)
	}
	return problems
}

// ErrChanged reports that a file changed after its Change was planned.
// Apply wraps it with the file's path; the caller says how to retry.
var ErrChanged = errors.New("changed while it was being updated")

// Apply rolls back already written files on failure. It refuses a configuration
// changed since the plan was prepared, rather than overwriting concurrent edits.
func Apply(changes []Change) error { return apply(changes, resolveTarget) }

// apply is Apply writing each change where writeTarget says: resolveTarget,
// or a stand-in a test gives to point a write somewhere else.
func apply(changes []Change, writeTarget func(path string) (string, error)) error {
	applied := []Change{}
	for _, c := range changes {
		current, err := os.ReadFile(c.Path)
		exists := err == nil
		if (err != nil && !os.IsNotExist(err)) || exists != c.Existed || string(current) != string(c.Before) {
			return errors.Join(fmt.Errorf("%s %w", c.Path, ErrChanged), rollback(applied))
		}
		target, err := writeTarget(c.Path)
		if err != nil {
			return errors.Join(fmt.Errorf("cannot update %s: %w", c.Path, err), rollback(applied))
		}
		if c.Delete && target != c.Path {
			// It became a link since it was planned: the file it points to
			// is kept, emptied, as for any link (see PlanRemovalOf).
			c.Delete = false
		}
		if c.Delete {
			if err = os.Remove(c.Path); err != nil {
				return errors.Join(fmt.Errorf("cannot remove %s: %w", c.Path, err), rollback(applied))
			}
			applied = append(applied, c)
			continue
		}
		undo, err := snapshot(target)
		if err != nil {
			return errors.Join(fmt.Errorf("cannot update %s: %w", c.Path, err), rollback(applied))
		}
		if err = writeFile(target, c.After, c.Mode); err != nil {
			// writeFile replaces the file only by its final rename, so a
			// failure left the file as it was: only directories it created
			// are taken back.
			undo.removeCreatedDirs()
			return errors.Join(fmt.Errorf("cannot update %s: %w", c.Path, err), rollback(applied))
		}
		// What the application will read is the file through c.Path, links
		// and all; a write that landed anywhere else is not a success, and
		// is taken back along with the earlier changes.
		if written, err := os.ReadFile(c.Path); err != nil || string(written) != string(c.After) {
			return errors.Join(fmt.Errorf("cannot update %s: the file written (%s) does not read back through it", c.Path, target), undo.restore(), rollback(applied))
		}
		applied = append(applied, c)
	}
	return nil
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

// Rollback undoes applied changes in reverse order: each file is restored to
// its Before content, or removed if the change created it. A file whose
// content is no longer the change's After was edited since, so it is left
// untouched and reported as needing manual recovery. All failures are joined
// into the returned error.
func Rollback(changes []Change) error { return rollback(changes) }

func rollback(changes []Change) error {
	var failures []error
	for i := len(changes) - 1; i >= 0; i-- {
		c := changes[i]
		if !Applied(c) {
			failures = append(failures, fmt.Errorf("%s changed; manual recovery required", c.Path))
			continue
		}
		var e error
		if c.Existed {
			e = atomicWrite(c.Path, c.Before, c.Mode)
		} else {
			// Remove what was created: through a symlink, its target.
			var target string
			if target, e = resolveTarget(c.Path); e == nil {
				e = os.Remove(target)
			}
		}
		if e != nil {
			failures = append(failures, e)
		}
	}
	return errors.Join(failures...)
}

// resolveTarget follows path through any symlinks to the file they name,
// which need not exist yet.
func resolveTarget(path string) (string, error) {
	for range 40 {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			// A missing file may still sit in a symlinked directory; that is
			// resolved by the rename itself.
			return path, nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return path, nil
		}
		link, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(link) {
			// Relative to the directory the link really sits in, which is
			// not filepath.Dir(path) when that directory is itself a link
			// (~/.claude -> dotfiles/claude).
			dir, err := filepath.EvalSymlinks(filepath.Dir(path))
			if err != nil {
				return "", err
			}
			link = filepath.Join(dir, link)
		}
		path = link
	}
	return "", fmt.Errorf("%s: too many levels of symbolic links", path)
}

// atomicWrite replaces the file at path with data by renaming a temporary
// file into place. When path is a symlink (a dotfile manager's link into a
// repository, say) it writes the file the link names, in that file's own
// directory, so the link survives and the repository copy is the one
// updated.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	target, err := resolveTarget(path)
	if err != nil {
		return err
	}
	return writeFile(target, data, mode)
}

// priorFile is what was at a path before Apply wrote it, so a write that
// must be taken back can be: the old content, or its absence together with
// the directories the write created.
type priorFile struct {
	path    string
	data    []byte
	mode    os.FileMode
	existed bool
	created []string // directories that did not exist, deepest first
}

func snapshot(path string) (priorFile, error) {
	prior := priorFile{path: path}
	info, err := os.Stat(path)
	switch {
	case err == nil:
		// A file that is there but cannot be read could not be put back,
		// so it is refused rather than overwritten.
		data, err := os.ReadFile(path)
		if err != nil {
			return priorFile{}, err
		}
		prior.mode, prior.data, prior.existed = info.Mode().Perm(), data, true
	case !os.IsNotExist(err):
		return priorFile{}, err
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(dir); err == nil || filepath.Dir(dir) == dir {
			break
		}
		prior.created = append(prior.created, dir)
	}
	return prior, nil
}

// restore puts the path back as snapshot found it. Directories it created
// are removed only while empty.
func (p priorFile) restore() error {
	if p.existed {
		return writeFile(p.path, p.data, p.mode)
	}
	if err := os.Remove(p.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	p.removeCreatedDirs()
	return nil
}

// removeCreatedDirs removes the directories a write created, while empty.
func (p priorFile) removeCreatedDirs() {
	for _, dir := range p.created {
		_ = os.Remove(dir) // fails, and keeps the directory, if it is not empty
	}
}

// writeFile atomically replaces the regular file at path (no link
// following: see atomicWrite) with data.
func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".archive-")
	if err != nil {
		return err
	}
	name := f.Name()
	// After a successful rename there is nothing left at name to remove.
	defer func() { _ = os.Remove(name) }()
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
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
