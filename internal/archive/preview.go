package archive

import (
	"time"
)

// RecordPreview holds only filtered display facts from one complete record.
type RecordPreview struct {
	Name     string
	Title    string
	Branch   string
	Activity time.Time
	Gaps     []CaptureGap
	Kind     TurnKind
	Command  string
}

// PreviewAccumulator keeps only filtered display facts and one pending slash
// command, so an assistant reply can establish that it was a human prompt.
type PreviewAccumulator struct {
	Labels   Labels
	Activity time.Time
	Gaps     []CaptureGap
	pending  string
}

// AddFacts combines bounded safe display facts without native interpretation.
func (a *PreviewAccumulator) AddFacts(p RecordPreview, head bool) {
	if p.Name != "" {
		a.Labels.Name = p.Name
	}
	if p.Branch != "" {
		a.Labels.Branch = p.Branch
	}
	if p.Activity.After(a.Activity) {
		a.Activity = p.Activity
	}
	if head && a.Labels.Title == "" {
		switch p.Kind {
		case TurnKindAssistant:
			if a.pending != "" {
				a.Labels.Title = a.pending
				a.pending = ""
			}
		case TurnKindHumanPrompt:
			a.pending = ""
			a.Labels.Title = p.Title
		case TurnKindLocalCommand:
			a.pending = p.Command
		case TurnKindToolResult, TurnKindHarnessMeta, TurnKindCommandOutput, TurnKindHarnessNotification:
		// Harness records do not answer or interrupt a pending slash command.
		case TurnKindShellCommand, TurnKindCompactSummary:
			a.pending = ""
		}
	}
	for _, g := range p.Gaps {
		found := false
		for _, old := range a.Gaps {
			if old.Code == g.Code {
				found = true
				break
			}
		}
		if !found {
			a.Gaps = append(a.Gaps, CaptureGap{Code: g.Code})
		}
	}
}
