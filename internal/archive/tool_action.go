package archive

import (
	"fmt"
	"slices"
	"strings"
)

// ToolActionKind classifies the retained meaning of a native invocation. Parsers own
// tool names and argument layouts; shared builders own paths and presentation.
type ToolActionKind string

// Tool action kinds describe common renderer behavior for already interpreted facts.
const (
	ToolActionGeneric ToolActionKind = "generic"
	ToolActionShell   ToolActionKind = "shell"
	ToolActionRead    ToolActionKind = "read"
	ToolActionEdit    ToolActionKind = "edit"
	ToolActionSearch  ToolActionKind = "search"
	ToolActionAgent   ToolActionKind = "agent"
	ToolActionPlan    ToolActionKind = "plan"
)

// ToolAction carries semantic invocation facts for shared paths and presentation.
type ToolAction struct {
	Kind      ToolActionKind
	Text      string
	Path      string
	Files     []string
	LineStart *int
	LineCount *int
	Prompt    bool
	Plan      *PlanUpdate
}

// PlanUpdate describes a native plan replacement or merge after parsing.
type PlanUpdate struct {
	Items []HandoffPlanItem
	Merge bool
}

func turnDisplayText(turn NormalizedTurn) string {
	if turn.PresentationKnown {
		return turn.PresentationText
	}
	return turn.Text
}

func actionSummary(a ToolAction, root string) string {
	switch a.Kind {
	case ToolActionShell, ToolActionGeneric:
		return firstLine(a.Text, handoffSummaryCap)
	case ToolActionRead:
		file := shownFile(a.Path, root)
		if file == "" {
			return ""
		}
		if a.LineStart != nil {
			if a.LineCount != nil {
				return fmt.Sprintf("%s (lines %d–%d)", file, *a.LineStart, *a.LineStart+*a.LineCount-1)
			}
			return fmt.Sprintf("%s (from line %d)", file, *a.LineStart)
		}
		return file
	case ToolActionEdit:
		files := make([]string, len(a.Files))
		for i, file := range a.Files {
			files[i] = shownFile(file, root)
		}
		return strings.Join(files, ", ")
	case ToolActionSearch:
		if a.Text != "" {
			if where := workspaceFile(a.Path, root); where != "" {
				return firstLine(a.Text+" in "+where, handoffSummaryCap)
			}
			return firstLine(a.Text, handoffSummaryCap)
		}
	case ToolActionAgent:
		limit := handoffSummaryCap
		if a.Prompt {
			limit = handoffAgentPromptCap
		}
		return firstLine(a.Text, limit)
	case ToolActionPlan:
		return "updated the plan"
	}
	return ""
}

func applyPlan(update *PlanUpdate, previous []HandoffPlanItem) []HandoffPlanItem {
	if update == nil {
		return nil
	}
	items := slices.Clone(update.Items)
	if !update.Merge {
		return slices.DeleteFunc(items, func(item HandoffPlanItem) bool { return item.Text == "" })
	}
	merged := slices.Clone(previous)
	for _, item := range items {
		index := -1
		if item.ID != "" {
			index = slices.IndexFunc(merged, func(prior HandoffPlanItem) bool { return prior.ID == item.ID })
		}
		if index < 0 {
			if item.Text != "" {
				merged = append(merged, item)
			}
			continue
		}
		if item.Text != "" {
			merged[index].Text = item.Text
		}
		if item.Status != "" {
			merged[index].Status = item.Status
		}
	}
	if merged == nil {
		merged = []HandoffPlanItem{}
	}
	return merged
}
