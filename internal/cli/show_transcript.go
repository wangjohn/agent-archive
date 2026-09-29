package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// transcriptOptions controls renderTranscript.
type transcriptOptions struct {
	summaryOptions
	// Full also prints each tool call's trimmed result.
	Full bool
}

// Per-result limits for `show --transcript --full`, the same head-and-tail
// trim handoff applies.
const (
	transcriptResultLines = 12
	transcriptResultBytes = 2000
)

// buildTranscript arranges a verified bundle for reading.
func buildTranscript(bundle archive.SourceBundle) (archive.Transcript, error) {
	return archive.BuildTranscript(bundle, archive.HandoffOptions{ToolResultLines: transcriptResultLines, ToolResultBytes: transcriptResultBytes})
}

// renderTranscript writes `show --transcript`: a short header, then each
// exchange's prompt (or app notice), the agent's replies, one line per tool
// call, and the shell and slash commands the person ran. The prompt is
// quoted with a gutter and each run of the agent's steps starts with the
// app's name, so where one speaker stops and the other starts shows without
// color. The transcript's strings are already display text
// (archive.BuildTranscript), so multi-line text is printed as it is.
func renderTranscript(w io.Writer, view sessionView, t archive.Transcript, opts transcriptOptions) {
	m := view.Metadata
	s := opts.Style
	agent := appName(archive.DisplayLine(m.Harness.Name))
	if agent == "" {
		agent = "Agent"
	}
	terminal.Println(w, s.bold(summaryTitle(m)))
	header := []string{}
	if agent := strings.TrimSpace(appName(archive.DisplayLine(m.Harness.Name)) + " " + archive.DisplayLine(m.Harness.Version)); agent != "" {
		header = append(header, agent)
	}
	if models := summaryModels(m.Models); len(models) > 0 {
		name, _, _ := strings.Cut(models[0], " · ")
		header = append(header, name)
	}
	if when := summaryWhen(m, opts.summaryOptions); when != "" {
		header = append(header, when)
	}
	terminal.Println(w, s.dim(strings.Join(header, " · ")))

	if len(t.Exchanges) == 0 {
		terminal.Println(w)
		terminal.Println(w, s.dim("(no conversation retained)"))
	}
	for _, exchange := range t.Exchanges {
		terminal.Println(w)
		var heading string
		switch exchange.Kind {
		case archive.TranscriptExchangePrompt:
			heading = "You"
		case archive.TranscriptExchangeNotification:
			heading = exchange.Text
		case archive.TranscriptExchangeLeading:
			heading = "Before the first prompt"
		}
		if at, err := time.Parse(time.RFC3339Nano, exchange.Timestamp); err == nil {
			heading += " · " + formatSummaryTime(at, opts.summaryOptions)
		}
		terminal.Println(w, s.bold("── "+heading+" "+strings.Repeat("─", max(4, 40-visibleWidth(heading)))))
		if exchange.Kind == archive.TranscriptExchangePrompt && exchange.Text != "" {
			renderPrompt(w, exchange.Text, s)
		}
		renderTranscriptSteps(w, exchange.Steps, agent, opts)
	}
	for _, final := range t.HookFinals {
		terminal.Println(w)
		terminal.Println(w, s.bold("── Final response (reported by the app's hook) ──"))
		terminal.Println(w, final)
	}
	if opts.Full && t.ToolResultsUnavailable {
		terminal.Println(w)
		terminal.Println(w, s.dim("This app records no tool results."))
	}
}

// promptGutter marks each line of the person's prompt.
const promptGutter = "┃"

// renderPrompt quotes the person's prompt with a gutter, wrapped to the
// style's width so a long line keeps its gutter where it breaks. Unlike
// hang, a word wider than the line (a link, a pasted blob, text with no
// spaces) is split, since the terminal would otherwise wrap it without the
// gutter.
func renderPrompt(w io.Writer, text string, s textStyle) {
	width := s.width
	if width > 0 {
		width = max(width-visibleWidth(promptGutter+" "), 1)
	}
	for line := range strings.SplitSeq(hangingIndent("", text, width), "\n") {
		for _, row := range splitColumns(line, width) {
			terminal.Println(w, strings.TrimRight(s.cmd(promptGutter)+" "+row, " "))
		}
	}
}

// splitColumns cuts line into rows at most width columns wide, never
// splitting a character; a wide character that would straddle the edge
// starts the next row. A width of 0 or less keeps the line whole.
func splitColumns(line string, width int) []string {
	if width <= 0 || visibleWidth(line) <= width {
		return []string{line}
	}
	var rows []string
	var row strings.Builder
	column := 0
	for _, r := range line {
		w := runeWidth(r)
		if column > 0 && column+w > width {
			rows = append(rows, row.String())
			row.Reset()
			column = 0
		}
		row.WriteRune(r)
		column += w
	}
	return append(rows, row.String())
}

// renderTranscriptSteps writes an exchange's steps. Each run of the agent's
// own steps (replies, tool calls, a compaction summary) starts with the
// agent's name; the person's shell and slash commands do not.
func renderTranscriptSteps(w io.Writer, steps []archive.TranscriptStep, agent string, opts transcriptOptions) {
	s := opts.Style
	// A blank line separates prose from the command lines around it; a run
	// of tool calls and commands stays together.
	previousLine, speaking := false, false
	for i, step := range steps {
		isLine := step.Kind != archive.TranscriptStepText && step.Kind != archive.TranscriptStepSummary
		isAgent := step.Kind == archive.TranscriptStepText || step.Kind == archive.TranscriptStepTool || step.Kind == archive.TranscriptStepSummary
		switch {
		case isAgent && !speaking:
			terminal.Println(w)
			terminal.Println(w, s.bold(agent+" ›"))
		case i == 0 || !isLine || !previousLine:
			terminal.Println(w)
		}
		previousLine, speaking = isLine, isAgent
		switch step.Kind {
		case archive.TranscriptStepText:
			terminal.Println(w, step.Text)
		case archive.TranscriptStepShell:
			// The person's own `!` command; its output only with --full.
			terminal.Println(w, "  "+s.cmd("$ "+firstTextLine(step.Text)))
			if opts.Full {
				renderOutput(w, step.Output, s)
			}
		case archive.TranscriptStepCommand:
			// A local slash command's output is the app's short reply
			// ("Set model to …"), so it is always shown.
			terminal.Println(w, "  "+s.cmd("» "+firstTextLine(step.Text)))
			renderOutput(w, step.Output, s)
		case archive.TranscriptStepOutput:
			if opts.Full {
				renderOutput(w, step.Output, s)
			}
		case archive.TranscriptStepSummary:
			terminal.Println(w, s.dim("[Compacted: the agent continued from this summary]"))
			terminal.Println(w, s.dim(step.Text))
		case archive.TranscriptStepTool:
			renderToolLine(w, step.Tool, opts)
		}
	}
}

func renderToolLine(w io.Writer, tool *archive.HandoffToolCall, opts transcriptOptions) {
	if tool == nil {
		return
	}
	s := opts.Style
	line := "  " + s.dim("▸") + " " + tool.Name
	if tool.Summary != "" {
		line += " " + tool.Summary
	}
	if tool.IsError {
		line += " " + s.failMark()
	}
	terminal.Println(w, line)
	if opts.Full {
		renderOutput(w, tool.Result, s)
	}
}

// renderOutput indents a result or command output under its line.
func renderOutput(w io.Writer, output string, s textStyle) {
	if output == "" {
		return
	}
	for line := range strings.SplitSeq(output, "\n") {
		terminal.Println(w, "      "+s.dim("│")+" "+line)
	}
}

// firstTextLine is text up to its first line break, marked "…" when more
// lines follow.
func firstTextLine(text string) string {
	line, rest, more := strings.Cut(strings.TrimSpace(text), "\n")
	if more && strings.TrimSpace(rest) != "" {
		return line + " …"
	}
	return line
}

// loadVerifiedSession downloads and verifies a session's source bundle
// against its metadata (reader.RefreshAndLoad), the only path by which show
// reads conversation content.
func loadVerifiedSession(ctx context.Context, store storage.ObjectStore, key string) (sessionView, archive.SourceBundle, error) {
	metadata, bundle, err := reader.RefreshAndLoad(ctx, store, key, reader.Limits{})
	if err != nil {
		return sessionView{}, archive.SourceBundle{}, err
	}
	return metadataWithLinks(ctx, store, metadata), bundle, nil
}

// describeBundleError is the message show prints when a verified bundle
// cannot be read: retention may have replaced or deleted it.
func describeBundleError(err error, sessionID, flag string) string {
	if errors.Is(err, reader.ErrRefreshRequired) {
		return fmt.Sprintf("the session's source bundle is not available (it may have just been replaced or deleted by retention); retry, or run `agent-archive show %s` without %s for its metadata", shellWord(archive.DisplayLine(sessionID)), flag)
	}
	return err.Error()
}
