package nativecodec

import (
	"github.com/wangjohn/agent-archive/internal/archive"
	"regexp"
	"strings"
)

var cursorTimestamp = regexp.MustCompile(`(?s)^\s*<timestamp>.*?</timestamp>\s*`)

// slashCommandName and slashCommandArgs read the tags Claude Code writes for a
// typed slash command: <command-name>/review-pr</command-name>,
// <command-message>…</command-message>, <command-args>12</command-args>.
var (
	slashCommandName = regexp.MustCompile(`(?s)<command-name>(.*?)</command-name>`)
	slashCommandArgs = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
)

// cleanPrompt shows a prompt as the person typed it. It removes the wrapper
// Cursor puts around a query (<timestamp>…</timestamp> then
// <user_query>…</user_query>), and turns Claude Code's slash-command tags
// back into the command line (/review-pr 12). Other prompts are only trimmed.
func cleanPrompt(text string) string {
	text = stripCursorWrapper(text)
	if strings.HasPrefix(strings.TrimSpace(text), "<command-") {
		if name := slashCommandName.FindStringSubmatch(text); name != nil {
			command := strings.TrimSpace(name[1])
			if args := slashCommandArgs.FindStringSubmatch(text); args != nil && strings.TrimSpace(args[1]) != "" {
				command += " " + strings.TrimSpace(args[1])
			}
			return command
		}
	}
	return strings.TrimSpace(text)
}

// stripHarnessTag removes a Claude Code harness wrapper such as
// <bash-input>…</bash-input> around a record's text.
func stripHarnessTag(text, tag string) string {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "<"+tag+">")
	return strings.TrimSuffix(text, "</"+tag+">")
}

var (
	// commandOutputTag matches the tags Claude Code wraps command output in.
	commandOutputTag = regexp.MustCompile(`(?s)<(bash-stdout|bash-stderr|local-command-stdout|local-command-stderr)>(.*?)</(?:bash-stdout|bash-stderr|local-command-stdout|local-command-stderr)>`)
	// taskStatus reads a background task notice's status.
	taskStatus = regexp.MustCompile(`(?s)<status>\s*(.*?)\s*</status>`)
)

// commandOutput is the text of a command-output record: stdout, then
// stderr, without their tags. The caveat Claude Code adds before local
// command output is not output.
func commandOutput(text string) string {
	matches := commandOutputTag.FindAllStringSubmatch(text, -1)
	if matches == nil {
		if strings.Contains(text, "<local-command-caveat>") {
			return ""
		}
		return strings.TrimSpace(text)
	}
	var parts []string
	for _, m := range matches {
		if body := strings.Trim(m[2], "\n"); strings.TrimSpace(body) != "" {
			parts = append(parts, body)
		}
	}
	return strings.Join(parts, "\n")
}

// notificationText describes a notice the app posted: "Background task
// completed" for Claude Code's task notification.
func notificationText(text string) string {
	if strings.Contains(text, "<task-notification>") {
		if m := taskStatus.FindStringSubmatch(text); m != nil && m[1] != "" {
			return "Background task " + firstLine(m[1], handoffSummaryCap)
		}
		return "Background task notification"
	}
	return "App notification"
}

func prepareTurnText(kind archive.TurnKind, text string) string {
	switch kind {
	case archive.TurnKindHumanPrompt, archive.TurnKindLocalCommand:
		return cleanPrompt(text)
	case archive.TurnKindShellCommand:
		return strings.TrimSpace(stripHarnessTag(text, "bash-input"))
	case archive.TurnKindCommandOutput:
		return commandOutput(text)
	case archive.TurnKindHarnessNotification:
		return notificationText(text)
	case archive.TurnKindAssistant, archive.TurnKindToolResult, archive.TurnKindHarnessMeta, archive.TurnKindCompactSummary:
		return text
	default:
		return text
	}
}

func firstLine(s string, limit int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i]) + " …"
	}
	if len(s) > limit {
		s = archive.TruncateUTF8(s, limit) + "…"
	}
	return s
}

const handoffSummaryCap = 200

// stripCursorWrapper removes native Cursor query framing without interpreting slash commands.
func stripCursorWrapper(text string) string {
	text = cursorTimestamp.ReplaceAllString(strings.TrimSpace(text), "")
	if strings.HasPrefix(text, "<user_query>") && strings.HasSuffix(text, "</user_query>") {
		text = stripHarnessTag(text, "user_query")
	}
	return text
}
