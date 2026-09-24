package archive

import (
	"fmt"
	"strings"
)

// HandoffRenderOptions controls markdown rendering.
type HandoffRenderOptions struct {
	// Preamble includes the note addressed to the receiving agent.
	Preamble bool
}

var harnessDisplayNames = map[string]string{"claude": "Claude Code", "codex": "Codex", "cursor": "Cursor"}

func harnessDisplayName(name string) string {
	if display := harnessDisplayNames[name]; display != "" {
		return display
	}
	if name == "" {
		return "coding agent"
	}
	return name
}

// RenderHandoffMarkdown renders h as a prompt another coding agent can
// continue from. Summary sections come before the conversation so an agent
// that reads only the top still has the essentials.
func RenderHandoffMarkdown(h Handoff, opts HandoffRenderOptions) []byte {
	var b strings.Builder
	source := h.Session.Source
	if source == "" {
		source = "unknown"
	}
	id := h.Session.ArchiveSessionID
	if id == "" {
		id = h.Session.NativeSessionID
	}
	fmt.Fprintf(&b, "<!-- agent-archive handoff v%d · %s · session %s · source: %s -->\n", HandoffVersion, h.Session.Harness, id, source)
	fmt.Fprintf(&b, "# Handoff: continuing a %s session\n\n", harnessDisplayName(h.Session.Harness))
	if opts.Preamble {
		b.WriteString("> You are picking up work another coding agent started. The conversation\n" +
			"> below is a filtered record: injected instructions and credentials were\n" +
			"> removed (a `[REDACTED]` marker is not a real value), tool output is\n" +
			"> trimmed, and edit bodies are omitted. Before acting, check the\n" +
			"> repository's current state (`git status`, the files listed below) rather\n" +
			"> than trusting the record. Ask the person if the next step is unclear.\n\n")
	}

	b.WriteString("## Session\n")
	agent := harnessDisplayName(h.Session.Harness)
	if h.Session.HarnessVersion != "" {
		agent += " " + h.Session.HarnessVersion
	}
	if len(h.Session.Models) > 0 {
		agent += " · models: " + strings.Join(h.Session.Models, ", ")
	}
	fmt.Fprintf(&b, "- Agent: %s\n", agent)
	var when []string
	if h.Session.StartedAt != nil {
		when = append(when, "started "+h.Session.StartedAt.Format("2006-01-02 15:04 UTC"))
	}
	if h.Session.LastActivityAt != nil {
		when = append(when, "last activity "+h.Session.LastActivityAt.Format("2006-01-02 15:04 UTC"))
	}
	if h.Session.State != "" && h.Session.State != MetadataStateUnknown {
		when = append(when, "state: "+string(h.Session.State))
	}
	if len(when) > 0 {
		fmt.Fprintf(&b, "- %s\n", capitalize(strings.Join(when, " · ")))
	}
	var where []string
	if h.Workspace.Branch != "" {
		where = append(where, "branch: "+h.Workspace.Branch)
	}
	if h.Workspace.Directory != "" {
		where = append(where, "directory: "+h.Workspace.Directory)
	}
	if len(where) > 0 {
		fmt.Fprintf(&b, "- %s (as recorded)\n", capitalize(strings.Join(where, " · ")))
	}
	if h.ToolResultsUnavailable {
		fmt.Fprintf(&b, "- %s does not record tool results, so none appear below.\n", agent)
	}
	b.WriteString("\n")

	if h.LeftOff != "" {
		fmt.Fprintf(&b, "## Where it left off\n%s\n\n", h.LeftOff)
	}
	if len(h.Plan) > 0 {
		b.WriteString("## Plan\n")
		for _, item := range h.Plan {
			box, suffix := "[ ]", ""
			switch strings.ToLower(item.Status) {
			case "completed", "done":
				box = "[x]"
			case "in_progress", "in-progress", "active":
				suffix = " (in progress)"
			}
			fmt.Fprintf(&b, "- %s %s%s\n", box, item.Text, suffix)
		}
		b.WriteString("\n")
	}
	if len(h.FilesTouched) > 0 {
		fmt.Fprintf(&b, "## Files touched\n%s\n\n", strings.Join(h.FilesTouched, ", "))
	}

	b.WriteString("## Conversation\n")
	for i, exchange := range h.Exchanges {
		if exchange.Prompt == "" {
			fmt.Fprintf(&b, "\n### %d · Before the first prompt\n", i+1)
		} else {
			fmt.Fprintf(&b, "\n### %d · Person\n", i+1)
			prompt := exchange.Prompt
			if exchange.PromptTruncated {
				prompt += " …(truncated)"
			}
			b.WriteString(quote(prompt))
		}
		inTools := false
		for _, step := range exchange.Steps {
			switch step.Kind {
			case "text":
				text := step.Text
				if step.TextTruncated {
					text += " …(shortened)"
				}
				fmt.Fprintf(&b, "\n**Agent:** %s\n", text)
				inTools = false
			case "shell":
				fmt.Fprintf(&b, "\n**Person ran:** `%s`\n", firstLine(step.Text, handoffSummaryCap))
				inTools = false
			case "summary":
				text := step.Text
				if step.TextTruncated {
					text += " …(shortened)"
				}
				fmt.Fprintf(&b, "\n**Conversation compacted.** The agent continued from this summary:\n\n%s", quote(text))
				inTools = false
			case "tool":
				if !inTools {
					b.WriteString("\n")
					inTools = true
				}
				renderTool(&b, step.Tool)
			case "collapsed":
				if !inTools {
					b.WriteString("\n")
					inTools = true
				}
				fmt.Fprintf(&b, "- %s\n", step.Text)
			}
		}
	}

	var footer []string
	if len(h.Elisions) > 0 {
		parts := make([]string, 0, len(h.Elisions))
		for _, e := range h.Elisions {
			parts = append(parts, describeElision(e))
		}
		footer = append(footer, "Omitted to fit the size limit: "+strings.Join(parts, "; ")+".")
		if h.FullRecordPath != "" {
			footer = append(footer, fmt.Sprintf("Full record: %s (read it for anything omitted here).", h.FullRecordPath))
		}
	}
	if len(h.Gaps) > 0 {
		parts := make([]string, 0, len(h.Gaps))
		for _, gap := range h.Gaps {
			parts = append(parts, fmt.Sprintf("%s ×%d", gap.Code, gap.Count))
		}
		footer = append(footer, "Capture gaps: "+strings.Join(parts, ", ")+".")
	}
	if len(footer) > 0 {
		b.WriteString("\n---\n")
		b.WriteString(strings.Join(footer, "\n"))
		b.WriteString("\n")
	}
	return []byte(b.String())
}

func renderTool(b *strings.Builder, tool *HandoffToolCall) {
	line := "- `" + tool.Name + "`"
	if tool.Summary != "" {
		line += " " + tool.Summary
	}
	switch {
	case tool.IsError:
		line += " → error"
	case tool.ResultOmitted:
		line += fmt.Sprintf(" → %d lines (output omitted)", tool.ResultLines)
	}
	b.WriteString(line + "\n")
	if tool.Result != "" {
		fence := codeFence(tool.Result)
		fmt.Fprintf(b, "  %s\n", fence)
		for _, resultLine := range strings.Split(tool.Result, "\n") {
			fmt.Fprintf(b, "  %s\n", resultLine)
		}
		fmt.Fprintf(b, "  %s\n", fence)
	}
}

// codeFence returns a backtick fence longer than any backtick run in text.
func codeFence(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	if longest < 3 {
		return "```"
	}
	return strings.Repeat("`", longest+1)
}

func quote(text string) string {
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			b.WriteString(">\n")
			continue
		}
		b.WriteString("> " + line + "\n")
	}
	return b.String()
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func describeElision(e HandoffElision) string {
	span := fmt.Sprintf("exchange %d", e.First)
	if e.Last != e.First {
		span = fmt.Sprintf("exchanges %d–%d", e.First, e.Last)
	}
	switch e.Kind {
	case HandoffElisionToolOutput:
		return fmt.Sprintf("output of %d tool calls in %s", e.Count, span)
	case HandoffElisionToolCalls:
		return fmt.Sprintf("%d tool calls in %s collapsed to counts", e.Count, span)
	case HandoffElisionAssistantText:
		return fmt.Sprintf("%d agent messages in %s shortened", e.Count, span)
	case HandoffElisionPromptText:
		return fmt.Sprintf("%d long prompts in %s truncated", e.Count, span)
	}
	return fmt.Sprintf("%d %s in %s", e.Count, e.Kind, span)
}
