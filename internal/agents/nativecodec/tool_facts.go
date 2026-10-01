package nativecodec

import (
	"encoding/json"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/archive"
	"strings"
)

var (
	shellToolNames  = map[string]bool{"bash": true, "shell": true, "exec_command": true, "local_shell_call": true, "commandexecution": true, "run_terminal_cmd": true}
	readToolNames   = map[string]bool{"read": true, "read_file": true, "read_file_v2": true, "view": true}
	editToolNames   = map[string]bool{"edit": true, "multiedit": true, "write": true, "notebookedit": true, "apply_patch": true, "str_replace": true, "strreplace": true, "search_replace": true, "edit_file": true, "edit_file_v2": true, "create_file": true, "write_file": true, "delete_file": true}
	searchToolNames = map[string]bool{"grep": true, "glob": true, "search": true, "codebase_search": true, "grep_search": true, "file_search": true, "ripgrep_raw_search": true, "glob_file_search": true}
	agentToolNames  = map[string]bool{"agent": true, "task": true}
	planToolNames   = map[string]bool{"todowrite": true, "todo_write": true, "update_plan": true}
)

var touchedPathKeys = []string{"file_path", "path", "target_file", "notebook_path", "relativeWorkspacePath"}

// codexExecCommand returns the cmd of the first exec_command({…}) call in a
// Codex exec script, or "" when there is none or it does not decode.
func codexExecCommand(script string) string {
	at := strings.Index(script, "exec_command(")
	if at < 0 {
		return ""
	}
	var args map[string]any
	if err := json.NewDecoder(strings.NewReader(script[at+len("exec_command("):])).Decode(&args); err != nil {
		return ""
	}
	return argumentText(args, "cmd", "command")
}

// argumentText returns the first named argument as text, joining a list of
// strings (Codex shell's argv) with spaces.
func argumentText(args map[string]any, keys ...string) string {
	for _, key := range keys {
		switch value := args[key].(type) {
		case string:
			if value != "" {
				return value
			}
		case []any:
			parts := make([]string, 0, len(value))
			for _, part := range value {
				if s, ok := part.(string); ok {
					parts = append(parts, s)
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, " ")
			}
		}
	}
	return ""
}

// touchedFiles lists the files an editing call names: its path argument, or
// for apply_patch the files named in the patch headers.
func touchedFiles(name string, input map[string]any, raw map[string]any) []string {
	lower := strings.ToLower(name)
	if !editToolNames[lower] {
		return nil
	}
	if file := firstString(input, touchedPathKeys...); file != "" {
		return []string{file}
	}
	patch := firstString(input, "input", "patch")
	if patch == "" {
		patch, _ = raw["input"].(string)
	}
	var out []string
	for line := range strings.SplitSeq(patch, "\n") {
		for _, prefix := range []string{"*** Update File: ", "*** Add File: ", "*** Delete File: ", "*** Move to: "} {
			if file, ok := strings.CutPrefix(line, prefix); ok {
				out = append(out, strings.TrimSpace(file))
			}
		}
	}
	return out
}

// replacementPlanItems reads the item list of a plan-writing call as a whole
// plan (see planItems).
func replacementPlanItems(name string, input map[string]any) []archive.HandoffPlanItem {
	if !planToolNames[strings.ToLower(name)] {
		return nil
	}
	var list []any
	found := false
	for _, key := range []string{"todos", "plan", "items"} {
		if value, ok := input[key].([]any); ok {
			list, found = value, true
			break
		}
	}
	if !found {
		// Arguments that did not decode, or a shape this reader does not
		// know, say nothing about the plan; they must not erase an earlier
		// one. An explicit empty list does clear it.
		return nil
	}
	items := []archive.HandoffPlanItem{}
	for _, raw := range list {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		item := archive.HandoffPlanItem{Text: firstString(entry, "content", "step", "description", "title", "text"), Status: firstString(entry, "status")}
		switch id := entry["id"].(type) {
		case string:
			item.ID = id
		case float64, json.Number:
			item.ID = fmt.Sprint(id)
		}
		// An item with no text is kept only for its id: a merge may update
		// the status of an item it names by id alone.
		if item.Text == "" && item.ID == "" {
			continue
		}
		items = append(items, item)
	}
	return items
}
func observedTool(call archive.NormalizedToolCall, raw map[string]any) archive.NormalizedToolCall {
	name := call.Name
	if name == "" {
		name = firstString(raw, "type")
	}
	call.ObservedName = name
	call.Invocation = toolInvocationTypes[strings.ToLower(strings.TrimSpace(firstString(raw, "type")))]
	input := call.Input
	lower := strings.ToLower(name)
	action := archive.ToolAction{}
	switch {
	case shellToolNames[lower]:
		action.Kind = archive.ToolActionShell
		action.Text = argumentText(input, "command", "cmd")
		if action.Text == "" {
			action.Text = argumentText(raw, "command")
		}
		call.ShellCommand = action.Text
	case readToolNames[lower]:
		action.Kind = archive.ToolActionRead
		action.Path = firstString(input, touchedPathKeys...)
		if offset, ok := input["offset"].(float64); ok {
			v := int(offset)
			action.LineStart = &v
			if limit, ok := input["limit"].(float64); ok {
				v := int(limit)
				action.LineCount = &v
			}
		}
	case editToolNames[lower]:
		action.Kind = archive.ToolActionEdit
		action.Files = touchedFiles(name, input, raw)
	case searchToolNames[lower]:
		action.Kind = archive.ToolActionSearch
		action.Text = firstString(input, "pattern", "query", "glob_pattern", "glob", "globPattern")
		action.Path = firstString(input, "path", "target_directory", "targetDirectory")
	case agentToolNames[lower]:
		action.Kind = archive.ToolActionAgent
		action.Text = firstString(input, "description")
		if action.Text == "" {
			action.Text = firstString(input, "prompt")
			action.Prompt = true
		}
	case planToolNames[lower]:
		action.Kind = archive.ToolActionPlan
		if items := replacementPlanItems(name, input); items != nil {
			merge, _ := input["merge"].(bool)
			action.Plan = &archive.PlanUpdate{Items: items, Merge: merge}
		}
	}
	// A specialized tool with no meaningful arguments uses the original generic fallback.
	if action.Text == "" && action.Path == "" && len(action.Files) == 0 && action.Kind != archive.ToolActionPlan && action.Kind != archive.ToolActionEdit {
		action.Kind = archive.ToolActionGeneric
		action.Text = firstString(input, "title")
		if action.Text == "" && input != nil {
			if encoded, err := json.Marshal(input); err == nil && string(encoded) != "{}" {
				action.Text = string(encoded)
			}
		}
		if action.Text == "" {
			if text, ok := raw["input"].(string); ok {
				action.Text = codexExecCommand(text)
				if action.Text == "" {
					action.Text = text
				}
			}
		}
	}
	if lower == "exec" {
		call.ShellCommand, _ = raw["input"].(string)
	}
	call.Action = action
	return call
}
