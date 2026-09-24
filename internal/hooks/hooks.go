// Package hooks owns lifecycle configuration only. It never trusts hooks or runs an agent.
package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Owner marks the hook handlers agent-archive installs, so setup and
// uninstall can find and replace or remove exactly their own entries and
// leave everything else alone. Cursor handlers carry it as a trailing shell
// comment on the command; other harnesses carry it as the statusMessage the
// application shows while the hook runs.
const Owner = "agent-archive lifecycle capture"

// prototypeOwner marks the handlers the pre-release prototype installed;
// setup and uninstall remove them like our own.
const prototypeOwner = "Recording private skill-run evidence"

// Hook is what setup installs into each application's hook configuration:
// the executable the hook runs and, when agent-archive's data directory is
// not the default, that directory. An application starts hooks with its own
// environment (Cursor launched from the Dock has none of the user's shell
// variables), so a relocated data directory must travel in the command
// itself, as the LaunchAgent's does.
type Hook struct {
	// Executable is the installed absolute path, not a developer checkout
	// or shell fragment.
	Executable string
	// DataHome is the AGENT_ARCHIVE_HOME the hook runs with, or "" for the
	// default data directory, which a hook finds without being told.
	DataHome string
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
	switch harness {
	case "codex":
		return []string{"SessionStart", "UserPromptSubmit", "Stop", "Interrupt", "SessionEnd", "SubagentStop"}, nil
	case "claude":
		return []string{"SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "SessionEnd", "SubagentStop"}, nil
	case "cursor":
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
	doc, err := parseDocument(existing)
	if err != nil {
		return nil, err
	}
	if harness == "cursor" {
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
	emptied, err := stripOwned(hs, harness)
	if err != nil {
		return nil, err
	}
	for event, empty := range emptied {
		// An event an earlier release installed and this one no longer uses.
		if empty && !contains(names, event) {
			hs.remove(event, 0)
		}
	}
	for _, event := range names {
		// Keys in the order earlier releases wrote them (sorted), so rerunning
		// setup over an existing installation changes nothing.
		handler := &object{members: []member{{"command", command}, {"timeout", json.Number("2")}}}
		var entry any = handler
		if harness != "cursor" {
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

// Remove strips every handler this tool installed for harness from existing,
// leaving unrelated handlers and top-level settings exactly as they were. It
// reports whether anything was actually removed so a caller can skip
// rewriting a file that never contained our entries. Like Merge, it only
// ever matches our own marker (or the exact known prototype handler), never
// a substring of an unrelated command. An event, or the whole "hooks"
// member, that only ever held our handlers is dropped, so a file setup only
// added hooks to gets its original bytes back.
func Remove(existing []byte, harness string) ([]byte, bool, error) {
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
	emptied, err := stripOwned(hs, harness)
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
	} else if err = doc.set("hooks", hs); err != nil {
		return nil, false, err
	}
	return doc.bytes(), true, nil
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
	return hs, nil
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

// owned reports whether a handler is ours (or the prototype's) and so
// something setup replaces and uninstall removes.
func owned(handler *object, harness string) bool {
	if harness == "cursor" {
		command, _ := handler.get("command")
		s, ok := command.(string)
		return ok && strings.HasSuffix(s, " # "+Owner)
	}
	status, _ := handler.get("statusMessage")
	return status == Owner || status == prototypeOwner
}

// stripOwned removes only our marker or an exact known prototype handler
// from every event in hs, including events no longer used by the current
// implementation. It never removes by substring alone. It returns each
// event it removed a handler from, mapped to whether that left the event
// empty.
func stripOwned(hs *object, harness string) (map[string]bool, error) {
	changed := map[string]bool{}
	for i, m := range hs.members {
		groups, ok := m.value.([]any)
		if !ok {
			return nil, fmt.Errorf("invalid hook list for %s", m.key)
		}
		kept := []any{}
		removed := false
		for _, item := range groups {
			g, ok := item.(*object)
			if !ok {
				return nil, errors.New("invalid hook entry")
			}
			if harness == "cursor" {
				if owned(g, harness) {
					removed = true
					continue
				}
				kept = append(kept, g)
				continue
			}
			rawHandlers, _ := g.get("hooks")
			handlers, ok := rawHandlers.([]any)
			if !ok {
				return nil, errors.New("invalid hook handlers")
			}
			remaining := []any{}
			for _, h := range handlers {
				handler, ok := h.(*object)
				if !ok {
					return nil, errors.New("invalid hook handler")
				}
				if owned(handler, harness) {
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

func contains(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}
