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
// exchange's prompt, the agent's replies, and one line per tool call. The
// transcript's strings are already display text (archive.BuildTranscript),
// so multi-line text is printed as it is.
func renderTranscript(w io.Writer, view sessionView, t archive.Transcript, opts transcriptOptions) {
	m := view.Metadata
	s := opts.Style
	title := strings.TrimSpace(archive.DisplayLine(m.Title))
	if title == "" {
		title = shortSessionID(m.SessionID)
	}
	terminal.Println(w, s.bold(title))
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
		heading := "You"
		if exchange.Prompt == "" {
			heading = "Before the first prompt"
		}
		if at, err := time.Parse(time.RFC3339Nano, exchange.Timestamp); err == nil {
			heading += " · " + formatSummaryTime(at, opts.summaryOptions)
		}
		terminal.Println(w, s.bold("── "+heading+" "+strings.Repeat("─", max(4, 40-visibleWidth(heading)))))
		if exchange.Prompt != "" {
			terminal.Println(w, exchange.Prompt)
		}
		renderTranscriptSteps(w, exchange.Steps, opts)
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

func renderTranscriptSteps(w io.Writer, steps []archive.HandoffStep, opts transcriptOptions) {
	s := opts.Style
	// A blank line separates prose from the tool lines around it; a run of
	// tool calls stays together.
	previousTool := false
	for i, step := range steps {
		isTool := step.Kind == archive.HandoffStepTool || step.Kind == archive.HandoffStepShell
		if i == 0 || !isTool || !previousTool {
			terminal.Println(w)
		}
		previousTool = isTool
		switch step.Kind {
		case archive.HandoffStepText:
			terminal.Println(w, step.Text)
		case archive.HandoffStepShell:
			terminal.Println(w, "  "+s.cmd("$ "+firstTextLine(step.Text)))
		case archive.HandoffStepSummary:
			terminal.Println(w, s.dim("[Compacted: the agent continued from this summary]"))
			terminal.Println(w, s.dim(step.Text))
		case archive.HandoffStepTool:
			renderToolLine(w, step.Tool, opts)
		case archive.HandoffStepCollapsed:
			terminal.Println(w, "  "+s.dim(step.Text))
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
	if !opts.Full || tool.Result == "" {
		return
	}
	for resultLine := range strings.SplitSeq(tool.Result, "\n") {
		terminal.Println(w, "      "+s.dim("│")+" "+resultLine)
	}
}

// firstTextLine is text up to its first line break.
func firstTextLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
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
		return fmt.Sprintf("the session's source bundle is not available (it may have just been replaced or deleted by retention); retry, or run `agent-archive show %s` without %s for its metadata", sessionID, flag)
	}
	return err.Error()
}
