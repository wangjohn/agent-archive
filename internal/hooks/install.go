package hooks

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Change is prepared before any mutation so callers can show a concrete plan.
type Change struct {
	Path    string
	Before  []byte
	After   []byte
	Existed bool
	Mode    os.FileMode
	// Delete is a removal that leaves the file with nothing in it (see
	// Empty): Apply deletes it instead of writing After, so a file setup
	// created goes away again. Only a regular file is deleted, never a link
	// a dotfile manager keeps.
	Delete bool `json:",omitempty"`
}

// Applied reports whether the file at c.Path is as c leaves it: deleted, or
// holding After.
func (c Change) Applied() bool {
	current, err := os.ReadFile(c.Path)
	if c.Delete {
		return os.IsNotExist(err)
	}
	return err == nil && string(current) == string(c.After)
}

// Unapplied reports whether the file at c.Path is as c found it: holding
// Before, or absent when it did not exist.
func (c Change) Unapplied() bool {
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
func ResolveFiles(userHome string, lookupEnv func(string) (string, bool)) Files {
	dir := func(variable, fallback string) string {
		if value, ok := lookupEnv(variable); ok && value != "" {
			if abs, err := filepath.Abs(value); err == nil {
				return abs
			}
		}
		return filepath.Join(userHome, fallback)
	}
	return Files{
		string(harnessClaude): filepath.Join(dir("CLAUDE_CONFIG_DIR", ".claude"), "settings.json"),
		string(harnessCodex):  filepath.Join(dir("CODEX_HOME", ".codex"), "hooks.json"),
		string(harnessCursor): filepath.Join(userHome, ".cursor", "hooks.json"),
	}
}

func (f Files) path(harness string) (string, error) {
	if _, err := events(harness); err != nil {
		return "", err
	}
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
		path, err := files.path(h)
		if err != nil {
			return nil, err
		}
		before, err := os.ReadFile(path)
		exists := err == nil
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("cannot read %s", path)
		}
		after, err := Merge(before, h, hook)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		mode := os.FileMode(0600)
		if exists {
			info, e := os.Stat(path)
			if e != nil {
				return nil, e
			}
			mode = info.Mode().Perm()
		}
		changes = append(changes, Change{Path: path, Before: before, After: after, Existed: exists, Mode: mode})
	}
	return changes, nil
}

// ErrChanged reports that a file changed after its Change was planned.
// Apply wraps it with the file's path; the caller says how to retry.
var ErrChanged = errors.New("changed while it was being updated")

// Apply rolls back already written files on failure. It refuses a configuration
// changed since the plan was prepared, rather than overwriting concurrent edits.
func Apply(changes []Change) error {
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
		if c.Delete {
			if err = os.Remove(target); err != nil {
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

// PlanRemoval prepares the inverse of Plan for uninstall: for each harness,
// a Change whose After is the current file with only hook's installation's
// handlers (and the prototype's) stripped (see Remove). A harness whose hook
// file is missing, or whose file never contained those, yields no Change at
// all, so an unrelated configuration, or one only another installation's
// hooks are in, is never rewritten or reformatted. Apply the result with
// Apply, which keeps its refuse-on-concurrent-edit and rollback behavior.
func PlanRemoval(files Files, hook Hook, harnesses []string) ([]Change, error) {
	changes := []Change{}
	for _, h := range harnesses {
		c, found, err := PlanRemovalOf(files, hook, h)
		if err != nil {
			return nil, err
		}
		if found {
			changes = append(changes, c)
		}
	}
	return changes, nil
}

// PlanRemovalOf is PlanRemoval for one harness; found is false when its file
// holds nothing of hook's installation.
func PlanRemovalOf(files Files, hook Hook, harness string) (change Change, found bool, err error) {
	path, err := files.path(harness)
	if err != nil {
		return Change{}, false, err
	}
	before, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Change{}, false, nil
	}
	if err != nil {
		return Change{}, false, fmt.Errorf("cannot read %s", path)
	}
	after, removed, err := Remove(before, harness, hook)
	if err != nil {
		return Change{}, false, fmt.Errorf("%s: %w", path, err)
	}
	if !removed {
		return Change{}, false, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return Change{}, false, err
	}
	link, err := os.Lstat(path)
	if err != nil {
		return Change{}, false, err
	}
	// A file left with nothing in it means what no file means, and is what
	// setup leaves of one it created, so it is deleted.
	remove := Empty(after) && link.Mode().IsRegular()
	return Change{Path: path, Before: before, After: after, Existed: true, Mode: info.Mode().Perm(), Delete: remove}, true, nil
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
		if !c.Applied() {
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

// writeTarget is where Apply writes a change to path: resolveTarget,
// replaceable so a test can make a write land in the wrong place.
var writeTarget = resolveTarget

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

// LaunchLabel is the background collector's launchd label for the default
// data directory.
const LaunchLabel = "com.agent-archive.collector"

// CollectorLabel is the launchd label of the collector for dataHome when it
// is not the account's default installation, which keeps LaunchLabel so an
// existing installation keeps its job. The label is derived from the
// directory: launchd labels are global to the login session, unlike HOME,
// so a test or secondary installation needs one of its own. defaultDataHome
// may be "" when the caller has already decided dataHome is not the default.
func CollectorLabel(dataHome, defaultDataHome string) string {
	if defaultDataHome != "" && filepath.Clean(dataHome) == filepath.Clean(defaultDataHome) {
		return LaunchLabel
	}
	sum := sha256.Sum256([]byte(filepath.Clean(dataHome)))
	return LaunchLabel + "." + hex.EncodeToString(sum[:])[:12]
}

// LaunchAgent is the collector's plist; label is its CollectorLabel.
func LaunchAgent(executable, dataHome, label string) ([]byte, error) {
	if !filepath.IsAbs(executable) || !filepath.IsAbs(dataHome) {
		return nil, errors.New("LaunchAgent paths must be absolute")
	}
	escape := func(s string) string {
		var b strings.Builder
		_ = xml.EscapeText(&b, []byte(s)) // a strings.Builder never fails to write
		return b.String()
	}
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>_collect</string></array>
<key>EnvironmentVariables</key><dict><key>AGENT_ARCHIVE_HOME</key><string>%s</string></dict>
<key>RunAtLoad</key><true/><key>StartInterval</key><integer>60</integer>
<key>ProcessType</key><string>Background</string>
<key>StandardOutPath</key><string>%s</string>
<key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, escape(label), escape(executable), escape(dataHome), escape(filepath.Join(dataHome, "collector.log")), escape(filepath.Join(dataHome, "collector-error.log")))), nil
}

// LaunchAgentDataHome returns the AGENT_ARCHIVE_HOME a LaunchAgent plist
// sets, or "" when it sets none.
func LaunchAgentDataHome(plist []byte) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(plist))
	decoder.Strict = false
	var (
		lastKey  string
		depth    int
		envDepth = -1
		reading  bool
		text     strings.Builder
	)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("read LaunchAgent: %w", err)
		}
		switch t := token.(type) {
		case xml.StartElement:
			depth++
			//lint:ignore LV1001 plist element names come from an external XML format; any other element is skipped
			switch t.Name.Local {
			case "key", "string":
				reading = true
				text.Reset()
			case "dict":
				if lastKey == "EnvironmentVariables" && envDepth < 0 {
					envDepth = depth
				}
			}
		case xml.CharData:
			if reading {
				text.Write(t)
			}
		case xml.EndElement:
			reading = false
			//lint:ignore LV1001 plist element names come from an external XML format; any other element is skipped
			switch t.Name.Local {
			case "key":
				lastKey = strings.TrimSpace(text.String())
			case "string":
				if envDepth >= 0 && depth == envDepth+1 && lastKey == "AGENT_ARCHIVE_HOME" {
					return strings.TrimSpace(text.String()), nil
				}
			case "dict":
				if depth == envDepth {
					envDepth = -1
				}
			}
			depth--
		}
	}
}

// LaunchAgentProgram returns the executable a LaunchAgent plist runs: the
// first ProgramArguments string. It reads any well-formed plist, not only one
// LaunchAgent wrote, and reports an error when there is no program to read.
func LaunchAgentProgram(plist []byte) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(plist))
	decoder.Strict = false
	var (
		lastKey      string
		inArguments  bool
		readingKey   bool
		readingValue bool
		text         strings.Builder
	)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return "", errors.New("LaunchAgent has no ProgramArguments")
		}
		if err != nil {
			return "", fmt.Errorf("read LaunchAgent: %w", err)
		}
		switch t := token.(type) {
		case xml.StartElement:
			//lint:ignore LV1001 plist element names come from an external XML format; any other element is skipped
			switch t.Name.Local {
			case "key":
				readingKey = true
				text.Reset()
			case "array":
				inArguments = lastKey == "ProgramArguments"
			case "string":
				readingValue = inArguments
				text.Reset()
			}
		case xml.CharData:
			if readingKey || readingValue {
				text.Write(t)
			}
		case xml.EndElement:
			//lint:ignore LV1001 plist element names come from an external XML format; any other element is skipped
			switch t.Name.Local {
			case "key":
				readingKey = false
				lastKey = strings.TrimSpace(text.String())
			case "string":
				if readingValue {
					if program := strings.TrimSpace(text.String()); program != "" {
						return program, nil
					}
					return "", errors.New("LaunchAgent program is empty")
				}
			case "array":
				if inArguments {
					return "", errors.New("LaunchAgent ProgramArguments is empty")
				}
			}
		}
	}
}

// Installed reports whether harness's hook file holds exactly what setup
// installs: in every lifecycle event, exactly one handler of hook's
// installation running hook's command, wherever it sits among the user's own
// handlers, and no handler of that installation (or the prototype) anywhere
// else. Formatting, key order, the user's own handlers, and another
// installation's do not affect the result (see OtherInstallations).
func Installed(files Files, hook Hook, harness string) (bool, error) {
	path, err := files.path(harness)
	if err != nil {
		return false, err
	}
	command, err := hook.Command(harness)
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	doc, err := parseDocument(data)
	if err != nil {
		return false, err
	}
	app := harnessName(harness)
	if app == harnessCursor {
		if v, _ := doc.root.get("version"); !isOne(v) {
			return false, nil
		}
	}
	hs, err := hooksObject(doc.root)
	if err != nil || hs == nil {
		return false, err
	}
	names, _ := events(harness)
	for _, event := range names {
		if _, ok := hs.get(event); !ok {
			return false, nil
		}
	}
	for _, m := range hs.members {
		groups, ok := m.value.([]any)
		if !ok {
			return false, fmt.Errorf("invalid hook list for %s", m.key)
		}
		handlers, err := handlerList(groups, app)
		if err != nil {
			return false, err
		}
		ours := 0
		for _, handler := range handlers {
			if kind, _, _ := classify(handler, app, hook); !hook.replaces(kind) {
				continue
			}
			got, _ := handler.get("command")
			kind, _ := handler.get("type")
			if got != command || (app != harnessCursor && kind != "command") {
				return false, nil
			}
			ours++
		}
		want := 0
		if slices.Contains(names, m.key) {
			want = 1
		}
		if ours != want {
			return false, nil
		}
	}
	return true, nil
}

// OtherInstallation is a hook handler another installation of agent-archive
// (another data directory) installed in a file this one uses.
type OtherInstallation struct {
	// DataHome is the data directory the handler runs with: its
	// AGENT_ARCHIVE_HOME, or the default installation's directory (""
	// when that is not known).
	DataHome string
	// Default is whether it is the account's default installation, whose
	// handlers set no AGENT_ARCHIVE_HOME.
	Default bool
	// Command is the handler's command when no data directory could be read
	// from it (it was edited by hand); DataHome is then "".
	Command string
}

// OtherInstallations lists, once each, the other installations whose
// handlers are in harness's hook file: handlers that carry agent-archive's
// marker but run with another data directory than hook's. Setup, uninstall,
// and Installed leave them alone; setup refuses to install beside them, since
// every session would then be captured twice. A missing file has none.
func OtherInstallations(files Files, hook Hook, harness string) ([]OtherInstallation, error) {
	path, err := files.path(harness)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	doc, err := parseDocument(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	hs, err := hooksObject(doc.root)
	if err != nil || hs == nil {
		return nil, err
	}
	app := harnessName(harness)
	var others []OtherInstallation
	for _, m := range hs.members {
		groups, ok := m.value.([]any)
		if !ok {
			return nil, fmt.Errorf("%s: invalid hook list for %s", path, m.key)
		}
		handlers, err := handlerList(groups, app)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for _, handler := range handlers {
			kind, dataHome, unreadable := classify(handler, app, hook)
			if kind != kindOther {
				continue
			}
			other := OtherInstallation{DataHome: dataHome, Command: unreadable}
			if other.Command == "" && other.DataHome == "" {
				other.DataHome, other.Default = hook.DefaultDataHome, true
			}
			if !slices.Contains(others, other) {
				others = append(others, other)
			}
		}
	}
	return others, nil
}
