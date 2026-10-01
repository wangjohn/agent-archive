package archive

import (
	"strings"
)

// harnessTextKinds classify a user record by the tag its text starts with.
// These records are written by the harness around something the person did,
// not typed as a prompt: Claude Code wraps a `!` shell command in
// <bash-input>, its output in <bash-stdout>/<bash-stderr>, a local command's
// output in <local-command-stdout>/<local-command-stderr>, the note it adds
// before local-command output in <local-command-caveat>, and a typed slash
// command in <command-name>/<command-message>/<command-args>.
// isPlaceholderModel reports a model name a harness writes on a message no
// model produced: Claude Code labels the messages it synthesizes itself
// (an interruption notice, "No response requested.") "<synthetic>". Such a
// name is not a model, and its usage is no model's usage.
func isPlaceholderModel(name string) bool {
	return strings.HasPrefix(name, "<") && strings.HasSuffix(name, ">")
}

// interruptionMarker is the user record Claude Code writes when the person
// stops a turn ("[Request interrupted by user]", "[Request interrupted by
// user for tool use]"). The harness writes it, not the person, so it is not
// a prompt and does not start an exchange.
// resolveSlashCommands decides which typed slash commands were prompts. A
// slash command that expands into a skill or custom command is answered by the
// assistant; a local one such as /model or /clear is answered only by
// local-command output. So a slash command counts as a prompt only if an
// assistant record follows before the next thing the person did, skipping
// harness-written records (isMeta expansions, command output, tool results).
// A compaction summary ends the scan without promoting: the conversation
// before it is summarized away, so an assistant record after it cannot be an
// answer to a command before it, and /compact itself is never a prompt.
func resolveSlashCommands(turns []NormalizedTurn) {
	for i := range turns {
		if turns[i].Kind != TurnKindLocalCommand {
			continue
		}
	scan:
		for _, next := range turns[i+1:] {
			switch next.Kind {
			case TurnKindAssistant:
				turns[i].Kind = TurnKindHumanPrompt
				break scan
			case TurnKindHumanPrompt, TurnKindLocalCommand, TurnKindShellCommand, TurnKindCompactSummary:
				break scan
			case TurnKindToolResult, TurnKindHarnessMeta, TurnKindCommandOutput, TurnKindHarnessNotification:
				// Keep looking for the answer.
			}
		}
	}
}

// textBlockTypes name the content blocks whose only payload is text. When the
// filter strips such a block's text — an injected <system-reminder> or
// <user_instructions> block is the common case — the block survives as a bare
// `{type: "text"}` that carries nothing a person sent.
