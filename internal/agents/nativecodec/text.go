package nativecodec

import (
	"github.com/wangjohn/agent-archive/internal/archive"
	"strings"
)

func parseText(a *archive.Analysis, b archive.SourceBundle) {
	for _, transcript := range b.NativeText {
		parsed, _ := parseTextSections(transcript.Content)
		for _, section := range parsed.sections {
			if !visibleTextRoles[section.role] {
				continue
			}
			parts := []string{strings.TrimSpace(section.header)}
			if len(section.lines) > 1 {
				parts = append(parts, section.lines[1:]...)
			}
			text := strings.TrimSpace(strings.Join(parts, "\n"))
			var kind archive.TurnKind
			switch section.role {
			case textRoleUser:
				kind = archive.TurnKindHumanPrompt
			case textRoleAssistant:
				kind = archive.TurnKindAssistant
			case textRoleTool:
				kind = archive.TurnKindToolResult
			case textRoleSystem, textRoleDeveloper, textRoleThinking, textRoleAnalysis:
				continue
			}
			a.View.Turns = append(a.View.Turns, archive.NormalizedTurn{RecordIndex: len(a.View.Turns), Role: string(section.role), Kind: kind, Text: text})
			if kind == archive.TurnKindHumanPrompt && a.Facts.TextTitle == "" {
				a.Facts.TextTitle = archive.CollapseSessionTitle(text)
			}
		}
	}
}
