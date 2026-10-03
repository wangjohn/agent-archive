package archive

import (
	"encoding/json"
	"errors"
	"time"
)

// RecordPreview holds only filtered display facts from one complete record.
type RecordPreview struct {
	Name     string
	Title    string
	Branch   string
	Activity time.Time
	Gaps     []CaptureGap
	kind     TurnKind
	command  string
}

// PreviewRecord shares the full privacy filter, without treating excerpts as
// whole conversations or retaining raw records after this call.
func PreviewRecord(harness string, record []byte) (RecordPreview, error) {
	var format string
	var known map[string]bool
	type previewHarness string
	switch previewHarness(harness) {
	case previewHarness(HarnessClaude):
		format = "claude-jsonl"
		known = map[string]bool{"user": true, "assistant": true, "tool_use": true, "tool_result": true, "message": true, "summary": true}
	case previewHarness(HarnessCodex):
		format = "codex-jsonl"
		known = map[string]bool{"session_meta": true, "turn_context": true, "response_item": true, "event_msg": true, "message": true, "token_usage_record": true}
	default:
		return RecordPreview{}, errors.New("unsupported preview harness")
	}
	read := false
	filtered, err := filterRecords(format, known, nil, func() ([]byte, bool) {
		if read {
			return nil, false
		}
		read = true
		return record, true
	}, func() error { return nil })
	if err != nil && !errors.Is(err, ErrUnsafeSourceFormat) {
		return RecordPreview{}, err
	}
	out := RecordPreview{Gaps: filtered.Gaps}
	for _, encoded := range filtered.Records {
		var safe map[string]any
		if err := json.Unmarshal(encoded, &safe); err != nil {
			return RecordPreview{}, err
		}
		if isSidechainRecord(safe) {
			continue
		}
		if kind, _ := safe["type"].(string); kind == string(claudeCustomTitleType) {
			out.Name = collapseSessionTitle(firstString(safe, "customTitle"))
		}
		if branch := validBranch(firstStringDeep(safe, "gitBranch")); branch != "HEAD" {
			out.Branch = branch
		}
		out.Activity = parseNativeTimestamp(safe)
		_, text, kind, ok := visibleMessage(safe)
		if ok {
			out.kind = refineUserKind(safe, kind, text)
			if out.kind == TurnKindHumanPrompt {
				out.Title = collapseSessionTitle(text)
			}
			if out.kind == TurnKindLocalCommand {
				out.command = collapseSessionTitle(text)
			}
		}
	}
	return out, nil
}

// PreviewAccumulator keeps only filtered display facts and one pending slash
// command, so an assistant reply can establish that it was a human prompt.
type PreviewAccumulator struct {
	Labels   Labels
	Activity time.Time
	Gaps     []CaptureGap
	pending  string
}

// Add applies the same prompt classification as normalized full transcripts.
// Tail records cannot manufacture a first prompt across an uninspected gap.
func (a *PreviewAccumulator) Add(harness string, record []byte, head bool) error {
	p, err := PreviewRecord(harness, record)
	if err != nil {
		return err
	}
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
		switch p.kind {
		case TurnKindAssistant:
			if a.pending != "" {
				a.Labels.Title = a.pending
				a.pending = ""
			}
		case TurnKindHumanPrompt:
			a.pending = ""
			a.Labels.Title = p.Title
		case TurnKindLocalCommand:
			a.pending = p.command
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
	return nil
}
