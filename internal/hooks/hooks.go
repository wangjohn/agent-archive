// Package hooks owns lifecycle configuration only. It never trusts hooks or runs an agent.
package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/wangjohn/agent-archive/internal/local"
)

// Owner marks the hook handlers agent-archive installs, so setup and
// uninstall can find and replace or remove exactly their own entries and
// leave everything else alone. Cursor handlers carry it as a trailing shell
// comment on the command; other harnesses carry it as the statusMessage the
// application shows while the hook runs.
const Owner = "agent-archive lifecycle capture"

// harnessName is an app whose hook configuration this package edits. The
// exported functions take its string form, as archive.Harness.Name holds it.
type harnessName string

const (
	harnessCodex  harnessName = "codex"
	harnessClaude harnessName = "claude"
	harnessCursor harnessName = "cursor"
)

// prototypeOwner marks the handlers the pre-release prototype installed;
// the default installation's setup and uninstall remove them like its own.
const prototypeOwner = "Recording private skill-run evidence"

// Hook is what setup installs into each application's hook configuration:
// the executable the hook runs and, when agent-archive's data directory is
// not the default, that directory. An application starts hooks with its own
// environment (Cursor launched from the Dock has none of the user's shell
// variables), so a relocated data directory must travel in the command
// itself, as the LaunchAgent's does.
//
// A Hook also names the installation whose handlers setup replaces and
// uninstall removes: each handler carries its data directory in its command
// (none for the default one), and a handler with another directory belongs
// to another installation and is never touched (see classify).
type Hook struct {
	// Executable is the installed absolute path, not a developer checkout
	// or shell fragment.
	Executable string
	// DataHome is the AGENT_ARCHIVE_HOME the hook runs with, or "" for the
	// default data directory, which a hook finds without being told.
	DataHome string
	// DefaultDataHome is the default installation's data directory, when
	// known: a handler without AGENT_ARCHIVE_HOME runs with it, so a Hook
	// whose DataHome names it explicitly is the same installation.
	DefaultDataHome string
}

// Command is the shell command installed for harness. Every path is single
// quoted, so no path can run as shell syntax.
func (h Hook) Command(harness string) (string, error) {
	if !filepath.IsAbs(h.Executable) || strings.ContainsAny(h.Executable, "\x00\r\n") {
		return "", errors.New("executable must be an absolute path")
	}
	if _, err := events(harness); err != nil {
		return "", err
	}
	command := quote(h.Executable) + " _hook --harness " + harness + " # " + Owner
	if h.DataHome == "" {
		return command, nil
	}
	if !filepath.IsAbs(h.DataHome) || strings.ContainsAny(h.DataHome, "\x00\r\n") {
		return "", errors.New("data directory must be an absolute path")
	}
	return "AGENT_ARCHIVE_HOME=" + quote(h.DataHome) + " " + command, nil
}

// events are the lifecycle events installed for harness.
func events(harness string) ([]string, error) {
	switch harnessName(harness) {
	case harnessCodex:
		return []string{"SessionStart", "UserPromptSubmit", "Stop", "Interrupt", "SessionEnd", "SubagentStop"}, nil
	case harnessClaude:
		return []string{"SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "SessionEnd", "SubagentStop"}, nil
	case harnessCursor:
		return []string{"sessionStart", "beforeSubmitPrompt", "afterAgentResponse", "stop", "sessionEnd", "subagentStop"}, nil
	}
	return nil, errors.New("unsupported harness")
}

// Merge preserves unrelated handlers and top-level settings: only the
// "hooks" member (and Cursor's "version") is rewritten, and everything else
// in the file keeps its bytes (see document).
func Merge(existing []byte, harness string, hook Hook) ([]byte, error) {
	command, err := hook.Command(harness)
	if err != nil {
		return nil, err
	}
	names, _ := events(harness)
	app := harnessName(harness)
	doc, err := parseDocument(existing)
	if err != nil {
		return nil, err
	}
	if app == harnessCursor {
		v, ok := doc.root.get("version")
		if ok && !isOne(v) {
			return nil, errors.New("unsupported Cursor hook configuration version")
		}
		if !ok {
			// Where Cursor's own files have it: before "hooks".
			if err = doc.setBefore("hooks", "version", json.Number("1")); err != nil {
				return nil, err
			}
		}
	}
	hs, err := hooksObject(doc.root)
	if err != nil {
		return nil, err
	}
	if hs == nil {
		hs = &object{}
	}
	emptied, err := stripOwned(hs, app, hook)
	if err != nil {
		return nil, err
	}
	for event, empty := range emptied {
		// An event an earlier release installed and this one no longer uses.
		if empty && !slices.Contains(names, event) {
			hs.remove(event, 0)
		}
	}
	for _, event := range names {
		// Keys in the order earlier releases wrote them (sorted), so rerunning
		// setup over an existing installation changes nothing.
		handler := &object{members: []member{{"command", command}, {"timeout", json.Number("2")}}}
		var entry any = handler
		if app != harnessCursor {
			handler = &object{members: []member{{"command", command}, {"statusMessage", Owner}, {"timeout", json.Number("2")}, {"type", "command"}}}
			entry = &object{members: []member{{"hooks", []any{handler}}}}
		}
		list, _ := getList(hs, event)
		hs.set(event, append(list, entry))
	}
	if err = doc.set("hooks", hs); err != nil {
		return nil, err
	}
	return doc.bytes(), nil
}

// Remove strips every handler hook's installation installed for harness
// from existing (and the prototype's), leaving unrelated handlers, another
// installation's, and top-level settings exactly as they were. It reports
// whether anything was actually removed so a caller can skip rewriting a
// file that never contained our entries. Like Merge, it only ever matches
// our own marker (or the exact known prototype handler), never a substring
// of an unrelated command. An event, or the whole "hooks" member, that only
// ever held our handlers is dropped, so a file setup only added hooks to
// gets its original bytes back. hook's Executable is not used.
func Remove(existing []byte, harness string, hook Hook) ([]byte, bool, error) {
	if _, err := events(harness); err != nil {
		return nil, false, err
	}
	if len(strings.TrimSpace(string(existing))) == 0 {
		return existing, false, nil
	}
	doc, err := parseDocument(existing)
	if err != nil {
		return nil, false, err
	}
	hs, err := hooksObject(doc.root)
	if err != nil {
		return nil, false, err
	}
	if hs == nil {
		return existing, false, nil
	}
	emptied, err := stripOwned(hs, harnessName(harness), hook)
	if err != nil {
		return nil, false, err
	}
	if len(emptied) == 0 {
		return existing, false, nil
	}
	for event, empty := range emptied {
		if empty {
			hs.remove(event, 0)
		}
	}
	if len(hs.members) == 0 {
		doc.remove("hooks")
		// Setup adds Cursor's "version" beside the hooks it installs; with
		// no hooks left and nothing else in the file, it goes too, and the
		// file is left as setup would have found an empty one.
		if v, ok := doc.root.get("version"); harnessName(harness) == harnessCursor && ok && isOne(v) && len(doc.root.members) == 1 {
			doc.clear()
		}
	} else if err = doc.set("hooks", hs); err != nil {
		return nil, false, err
	}
	return doc.bytes(), true, nil
}

// Empty reports whether data is a hook file with nothing in it: no settings
// at all, just an empty JSON object (what Remove leaves of a file setup
// created). Such a file means the same to every application as no file.
func Empty(data []byte) bool {
	if len(bytes.TrimSpace(data)) == 0 {
		return true
	}
	doc, err := parseDocument(data)
	return err == nil && len(doc.root.members) == 0
}

// hooksObject returns the "hooks" member, or nil when there is none (or it
// is null).
func hooksObject(root *object) (*object, error) {
	raw, ok := root.get("hooks")
	if !ok || raw == nil {
		return nil, nil
	}
	hs, ok := raw.(*object)
	if !ok {
		return nil, errors.New("invalid hooks object")
	}
	// The applications keep the last of two same-named keys; editing one of
	// them would silently drop the other's handlers, as it would for the
	// top-level "hooks" key itself (see parseDocument).
	if where, key, dup := duplicateKey(hs, `"hooks"`); dup {
		return nil, fmt.Errorf("%w: %s has more than one %q key; remove the duplicate", errInvalidConfiguration, where, key)
	}
	return hs, nil
}

// duplicateKey finds an object anywhere in value that names one key twice,
// and says where it is (from where) and which key.
func duplicateKey(value any, where string) (at, key string, found bool) {
	switch v := value.(type) {
	case *object:
		seen := map[string]bool{}
		for _, m := range v.members {
			if seen[m.key] {
				return where, m.key, true
			}
			seen[m.key] = true
		}
		for _, m := range v.members {
			if at, key, found = duplicateKey(m.value, fmt.Sprintf("%s.%q", where, m.key)); found {
				return at, key, true
			}
		}
	case []any:
		for i, item := range v {
			if at, key, found = duplicateKey(item, fmt.Sprintf("%s[%d]", where, i)); found {
				return at, key, true
			}
		}
	}
	return "", "", false
}

func getList(hs *object, event string) ([]any, error) {
	raw, _ := hs.get(event)
	list, ok := raw.([]any)
	if !ok && raw != nil {
		return nil, fmt.Errorf("invalid hook list for %s", event)
	}
	return list, nil
}

func isOne(v any) bool {
	n, ok := v.(json.Number)
	if !ok {
		return false
	}
	f, err := n.Float64()
	return err == nil && f == 1
}

// handlerKind is whose a hook handler is.
type handlerKind int

const (
	// kindUnrelated is a handler agent-archive did not install.
	kindUnrelated handlerKind = iota
	// kindOwn is one this installation installed.
	kindOwn
	// kindPrototype is one the pre-release prototype installed; every
	// installation replaces and removes it.
	kindPrototype
	// kindOther is one another installation (another data directory)
	// installed, which is never touched.
	kindOther
)

// classify says whose handler is. For kindOther it also returns the data
// directory the handler's command runs with ("" for the default one), or,
// when that cannot be read from the command, the command in unreadable.
func classify(handler *object, app harnessName, hook Hook) (kind handlerKind, dataHome, unreadable string) {
	command, _ := handler.get("command")
	text, _ := command.(string)
	if app == harnessCursor {
		if !strings.HasSuffix(text, " # "+Owner) {
			return kindUnrelated, "", ""
		}
	} else {
		status, _ := handler.get("statusMessage")
		if status == prototypeOwner {
			return kindPrototype, "", ""
		}
		if status != Owner {
			return kindUnrelated, "", ""
		}
	}
	dataHome, ok := CommandDataHome(text)
	if !ok {
		return kindOther, "", text
	}
	if hook.sameInstallation(dataHome) {
		return kindOwn, dataHome, ""
	}
	return kindOther, dataHome, ""
}

// CommandDataHome reads the data directory a hook command setup wrote runs
// with: the AGENT_ARCHIVE_HOME it sets, or "" when it sets none and so runs
// with the default one. ok is false when the command sets the variable in a
// form Command never writes.
func CommandDataHome(command string) (dataHome string, ok bool) {
	rest, found := strings.CutPrefix(command, "AGENT_ARCHIVE_HOME=")
	if !found {
		return "", true
	}
	// Command writes one single-quoted word, each ' in the path written as
	// '"'"' (see quote), followed by a space.
	var b strings.Builder
	for {
		switch {
		case strings.HasPrefix(rest, "'"):
			end := strings.IndexByte(rest[1:], '\'')
			if end < 0 {
				return "", false
			}
			b.WriteString(rest[1 : end+1])
			rest = rest[end+2:]
		case strings.HasPrefix(rest, `"'"`):
			b.WriteByte('\'')
			rest = rest[3:]
		case strings.HasPrefix(rest, " ") && filepath.IsAbs(b.String()):
			return b.String(), true
		default:
			return "", false
		}
	}
}

// sameInstallation reports whether a handler whose command runs with
// dataHome ("" for the default data directory) belongs to h's installation.
// Both sides are compared with their symlinks resolved, and "" stands for
// DefaultDataHome, so the default installation is one installation however
// its directory was spelled. Two spellings of one existing directory that
// differ only in case (APFS and HFS+ are case-insensitive by default) are
// one directory too.
func (h Hook) sameInstallation(dataHome string) bool {
	resolve := func(dir string) string {
		if dir == "" {
			dir = h.DefaultDataHome
		}
		if dir == "" {
			return ""
		}
		if resolved, err := local.ResolveExistingSymlinks(dir); err == nil {
			return resolved
		}
		return filepath.Clean(dir)
	}
	theirs, ours := resolve(dataHome), resolve(h.DataHome)
	if theirs == ours {
		return true
	}
	if theirs == "" || ours == "" {
		return false
	}
	a, errA := os.Stat(theirs)
	b, errB := os.Stat(ours)
	return errA == nil && errB == nil && os.SameFile(a, b)
}

// handlerList returns the handlers of one event's list: for Cursor the
// entries themselves, for the others the handlers inside each matcher group.
// It fails on a list of any other shape.
func handlerList(groups []any, app harnessName) ([]*object, error) {
	var out []*object
	for _, item := range groups {
		g, ok := item.(*object)
		if !ok {
			return nil, errors.New("invalid hook entry")
		}
		if app == harnessCursor {
			out = append(out, g)
			continue
		}
		raw, _ := g.get("hooks")
		handlers, ok := raw.([]any)
		if !ok {
			return nil, errors.New("invalid hook handlers")
		}
		for _, h := range handlers {
			handler, ok := h.(*object)
			if !ok {
				return nil, errors.New("invalid hook handler")
			}
			out = append(out, handler)
		}
	}
	return out, nil
}

// replaces reports whether setup replaces (and uninstall removes) a handler
// of kind for h's installation: its own, never another installation's, and
// the prototype's only for the default installation, which is the one that
// succeeds it (setup retires the prototype's job only there too). A second
// or test installation leaves the prototype as it is.
func (h Hook) replaces(kind handlerKind) bool {
	return kind == kindOwn || (kind == kindPrototype && h.DataHome == "")
}

// stripOwned removes this installation's handlers and exact known prototype
// handlers from every event in hs, including events no longer used by the
// current implementation. It never removes by substring alone, and never
// another installation's handler. It returns each event it removed a handler
// from, mapped to whether that left the event empty.
func stripOwned(hs *object, app harnessName, hook Hook) (map[string]bool, error) {
	changed := map[string]bool{}
	for i, m := range hs.members {
		groups, ok := m.value.([]any)
		if !ok {
			return nil, fmt.Errorf("invalid hook list for %s", m.key)
		}
		// Checks the shape of the whole list before anything is changed, so
		// the type assertions below cannot fail.
		if _, err := handlerList(groups, app); err != nil {
			return nil, err
		}
		kept := []any{}
		removed := false
		for _, item := range groups {
			g, _ := item.(*object)
			if app == harnessCursor {
				if kind, _, _ := classify(g, app, hook); hook.replaces(kind) {
					removed = true
					continue
				}
				kept = append(kept, g)
				continue
			}
			rawHandlers, _ := g.get("hooks")
			handlers, _ := rawHandlers.([]any)
			remaining := []any{}
			for _, h := range handlers {
				handler, _ := h.(*object)
				if kind, _, _ := classify(handler, app, hook); hook.replaces(kind) {
					removed = true
					continue
				}
				remaining = append(remaining, h)
			}
			if len(remaining) == len(handlers) {
				kept = append(kept, g)
			} else if len(remaining) > 0 {
				g.set("hooks", remaining)
				kept = append(kept, g)
			}
		}
		if removed {
			hs.members[i].value = kept
			changed[m.key] = len(kept) == 0
		}
	}
	return changed, nil
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
