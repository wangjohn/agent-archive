package archive

import (
	"strings"
)

// isPlaceholderModel reports a model name a harness writes on a message no
// model produced: Claude Code labels the messages it synthesizes itself
// (an interruption notice, "No response requested.") "<synthetic>". Such a
// name is not a model, and its usage is no model's usage.
func isPlaceholderModel(name string) bool {
	return strings.HasPrefix(name, "<") && strings.HasSuffix(name, ">")
}

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
