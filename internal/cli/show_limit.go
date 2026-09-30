package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// trimKind names one step of trimming `show --transcript --json`.
type trimKind string

// The kinds of trimming `show --transcript --json --max-bytes` records in
// the output's "trimmed" object, in the order they are applied. Each goes to
// the oldest records first and to no more of them than needed.
const (
	trimToolResults   trimKind = "tool_results"
	trimHookFinals    trimKind = "hook_finals"
	trimToolInput     trimKind = "tool_input"
	trimAssistantText trimKind = "assistant_text"
	trimPromptText    trimKind = "prompt_text"
	trimOldestRecords trimKind = "oldest_records"
)

// normalizedTurn is a NormalizedTurn as `show --transcript --json` prints
// it: TextTruncated is true when --max-bytes cut its text short.
type normalizedTurn struct {
	archive.NormalizedTurn
	TextTruncated bool `json:"text_truncated,omitempty"`
}

// trimmedOutput is the "trimmed" object of the normalized view, present
// only when --max-bytes trimmed it: the limit, what was omitted, and where
// the untrimmed output was saved (absent when it could not be).
type trimmedOutput struct {
	MaxBytes   int              `json:"max_bytes"`
	Omitted    []trimmedOmitted `json:"omitted"`
	FullRecord string           `json:"full_record,omitempty"`
}

// trimmedOmitted counts what one trimming step removed or shortened.
type trimmedOmitted struct {
	Kind  trimKind `json:"kind"`
	Count int      `json:"count"`
}

// showFullPath names where the untrimmed output of `show --transcript` for a
// bundle is saved: beside the untrimmed handoffs, which the same seven-day
// pruning covers, under a name of its own so neither overwrites the other.
// The readable transcript with --full holds more than without it, so it has
// a name of its own too: a path named at the end of one run never comes to
// hold what another run saved.
func showFullPath(home string, bundle archive.SourceBundle, jsonOut, full bool) string {
	base := strings.TrimSuffix(handoffFullPath(home, bundle, "markdown"), ".md") + ".transcript"
	switch {
	case jsonOut:
		return base + ".json"
	case full:
		return base + "-full.txt"
	}
	return base + ".txt"
}

// saveFullTranscript saves the untrimmed output, returning whether it is
// there for the footer to name.
func saveFullTranscript(path string, data []byte, stderr io.Writer) bool {
	if err := local.WriteBytes(path, data); err != nil {
		terminal.Printf(stderr, "agent-archive: show: warning: could not save the untrimmed transcript: %v\n", err)
		return false
	}
	return true
}

// warnOverLimit says the output is still over --max-bytes after every
// trimming step.
func warnOverLimit(stderr io.Writer, size, maxBytes int) {
	terminal.Printf(stderr, "agent-archive: show: warning: still %d bytes after trimming, over the %d-byte limit\n", size, maxBytes)
}

// renderTranscriptBytes is what renderTranscript writes.
func renderTranscriptBytes(view sessionView, t archive.Transcript, opts transcriptOptions) []byte {
	var buf bytes.Buffer
	renderTranscript(&buf, view, t, opts)
	return buf.Bytes()
}

// fitTranscriptToLimit trims t to maxBytes as it will be rendered with opts
// (FullRecord is filled in here), and saves the untrimmed rendering under
// home when it had to. It returns the transcript to print and the options to
// print it with.
func fitTranscriptToLimit(view sessionView, t archive.Transcript, bundle archive.SourceBundle, opts transcriptOptions, maxBytes int, home string, stderr io.Writer) (archive.Transcript, transcriptOptions) {
	path := showFullPath(home, bundle, false, opts.Full)
	withPath := opts
	withPath.FullRecord = path
	fitted, fits := archive.FitTranscript(t, maxBytes, opts.Full, func(x archive.Transcript) int {
		return len(renderTranscriptBytes(view, x, withPath))
	})
	if len(fitted.Elisions) > 0 {
		// The saved copy is plain text, whatever the terminal shows.
		plain := opts
		plain.Style = textStyle{}
		if saveFullTranscript(path, renderTranscriptBytes(view, t, plain), stderr) {
			opts.FullRecord = path
		}
	}
	if !fits {
		warnOverLimit(stderr, len(renderTranscriptBytes(view, fitted, opts)), maxBytes)
	}
	return fitted, opts
}

// normalizedDocuments is what `show --transcript --json` prints: the
// sidecar document, then the normalized view's.
func normalizedDocuments(view sessionView, n normalizedOutput) ([]byte, error) {
	first, err := jsonDocument(view)
	if err != nil {
		return nil, err
	}
	second, err := jsonDocument(n)
	if err != nil {
		return nil, err
	}
	return append(first, second...), nil
}

// fitNormalizedToLimit trims the normalized view so that both documents fit
// in maxBytes, and saves the untrimmed pair under home when it had to. See
// fitNormalized for what is trimmed.
func fitNormalizedToLimit(view sessionView, n normalizedOutput, bundle archive.SourceBundle, maxBytes int, home string, stderr io.Writer) (normalizedOutput, error) {
	path := showFullPath(home, bundle, true, false)
	measure := func(x normalizedOutput) int {
		data, err := normalizedDocuments(view, x)
		if err != nil {
			return 0
		}
		return len(data)
	}
	full, err := normalizedDocuments(view, n)
	if err != nil {
		return n, err
	}
	fitted, fits := fitNormalized(n, maxBytes, path, measure)
	if fitted.Trimmed != nil && !saveFullTranscript(path, full, stderr) {
		fitted.Trimmed.FullRecord = ""
	}
	if !fits {
		size, err := normalizedDocuments(view, fitted)
		if err != nil {
			return n, err
		}
		warnOverLimit(stderr, len(size), maxBytes)
	}
	return fitted, nil
}

// fitNormalized returns a copy of n trimmed until measure(copy) is at most
// maxBytes. It applies these steps in order, each to the oldest entries
// first and to no more of them than needed, recording each in Trimmed:
//
//  1. drop entries of tool_results (they hold sizes only; each call's
//     output_bytes says the same for a linked result);
//  2. drop entries of hook_finals (statuses only, no text; Claude Code
//     reports one per turn);
//  3. drop the input of tool calls;
//  4. cut the text of turns other than the person's prompts to 300 bytes,
//     setting text_truncated;
//  5. cut prompts to 2000 bytes, setting text_truncated;
//  6. drop the oldest turns, tool calls, and tool results altogether.
//
// Every prompt survives until the last step, and the output stays valid
// JSON. fits is false when the result is still over budget after every step.
// path is recorded as the untrimmed output's location; the caller clears it
// when saving fails.
func fitNormalized(n normalizedOutput, maxBytes int, path string, measure func(normalizedOutput) int) (normalizedOutput, bool) {
	out := cloneNormalized(n)
	if maxBytes <= 0 || measure(out) <= maxBytes {
		return out, true
	}
	record := func(trial normalizedOutput, kind trimKind, count int) normalizedOutput {
		if count == 0 {
			return trial
		}
		if trial.Trimmed == nil {
			trial.Trimmed = &trimmedOutput{MaxBytes: maxBytes, FullRecord: path}
		}
		trial.Trimmed.Omitted = append(trial.Trimmed.Omitted, trimmedOmitted{Kind: kind, Count: count})
		return trial
	}
	steps := []struct {
		kind  trimKind
		size  func(normalizedOutput) int
		apply func(trial *normalizedOutput, k int) int
	}{
		{trimToolResults, func(x normalizedOutput) int { return len(x.ToolResults) }, func(trial *normalizedOutput, k int) int {
			trial.ToolResults = trial.ToolResults[k:]
			return k
		}},
		{trimHookFinals, func(x normalizedOutput) int { return len(x.HookFinals) }, func(trial *normalizedOutput, k int) int {
			trial.HookFinals = trial.HookFinals[k:]
			return k
		}},
		{trimToolInput, func(x normalizedOutput) int { return len(x.ToolCalls) }, func(trial *normalizedOutput, k int) int {
			count := 0
			for i := range k {
				if trial.ToolCalls[i].Input != nil {
					trial.ToolCalls[i].Input = nil
					count++
				}
			}
			return count
		}},
		{trimAssistantText, func(x normalizedOutput) int { return len(x.Turns) }, func(trial *normalizedOutput, k int) int {
			return truncateTurns(trial.Turns[:k], false, 300)
		}},
		{trimPromptText, func(x normalizedOutput) int { return len(x.Turns) }, func(trial *normalizedOutput, k int) int {
			return truncateTurns(trial.Turns[:k], true, 2000)
		}},
		{trimOldestRecords, func(x normalizedOutput) int { return len(recordIndexes(x)) }, func(trial *normalizedOutput, k int) int {
			return dropOldestRecords(trial, k)
		}},
	}
	for _, step := range steps {
		before := out
		var fits bool
		out, fits = archive.SmallestFit(step.size(before), maxBytes, measure, func(k int) normalizedOutput {
			trial := cloneNormalized(before)
			return record(trial, step.kind, step.apply(&trial, k))
		})
		if fits {
			return out, true
		}
	}
	return out, false
}

// truncateTurns cuts the text of the turns that are (or, when prompts is
// false, are not) the person's prompts to limit bytes, and returns how many
// it cut.
func truncateTurns(turns []normalizedTurn, prompts bool, limit int) int {
	count := 0
	for i := range turns {
		if (turns[i].Kind == archive.TurnKindHumanPrompt) == prompts && len(turns[i].Text) > limit {
			turns[i].Text, turns[i].TextTruncated = archive.TruncateUTF8(turns[i].Text, limit), true
			count++
		}
	}
	return count
}

// recordIndexes lists, in order and once each, the record positions of the
// turns and tool calls that fitNormalized's last step may drop.
func recordIndexes(n normalizedOutput) []int {
	seen := map[int]bool{}
	var indexes []int
	add := func(i int) {
		if !seen[i] {
			seen[i] = true
			indexes = append(indexes, i)
		}
	}
	for _, turn := range n.Turns {
		add(turn.RecordIndex)
	}
	for _, call := range n.ToolCalls {
		add(call.RecordIndex)
	}
	for _, result := range n.ToolResults {
		add(result.RecordIndex)
	}
	sort.Ints(indexes)
	return indexes
}

// dropOldestRecords removes everything at the oldest k record positions and
// returns how many entries that removed.
func dropOldestRecords(n *normalizedOutput, k int) int {
	indexes := recordIndexes(*n)
	if k == 0 {
		return 0
	}
	cutoff := indexes[min(k, len(indexes))-1]
	before := len(n.Turns) + len(n.ToolCalls) + len(n.ToolResults)
	n.Turns = slices.DeleteFunc(n.Turns, func(t normalizedTurn) bool { return t.RecordIndex <= cutoff })
	n.ToolCalls = slices.DeleteFunc(n.ToolCalls, func(c archive.NormalizedToolCall) bool { return c.RecordIndex <= cutoff })
	n.ToolResults = slices.DeleteFunc(n.ToolResults, func(r archive.NormalizedToolResult) bool { return r.RecordIndex <= cutoff })
	return before - len(n.Turns) - len(n.ToolCalls) - len(n.ToolResults)
}

// cloneNormalized copies what fitNormalized changes.
func cloneNormalized(n normalizedOutput) normalizedOutput {
	out := n
	// slices.Clone keeps an empty list empty rather than nil, which would
	// print as null.
	out.Turns = slices.Clone(n.Turns)
	out.ToolCalls = slices.Clone(n.ToolCalls)
	out.ToolResults = slices.Clone(n.ToolResults)
	out.HookFinals = slices.Clone(n.HookFinals)
	if n.Trimmed != nil {
		trimmed := *n.Trimmed
		trimmed.Omitted = append([]trimmedOmitted(nil), n.Trimmed.Omitted...)
		out.Trimmed = &trimmed
	}
	return out
}

// jsonDocument is value as indented JSON with a final newline. Its strings
// come from bucket metadata, so the text goes through archive.DisplayJSON: a
// C1 control or bidi override is written as a \u escape, never raw to the
// terminal.
func jsonDocument(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode output: %w", err)
	}
	return append(archive.DisplayJSON(data), '\n'), nil
}
