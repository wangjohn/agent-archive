package archive

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Parser 0.8: a compaction summary is not a prompt or a message, /compact is
// still a local command, and each compaction is counted once.
func TestParserV08CompactionSummariesAreNotPrompts(t *testing.T) {
	view, metadata := parsedFixture(t, "claude", "claude-compaction.jsonl")
	countIs(t, "turns", metadata.Counts.Turns, 2)
	countIs(t, "messages", metadata.Counts.Messages, 5)
	countIs(t, "compactions", metadata.Counts.Compactions, 2)
	if len(metadata.Models) != 1 || metadata.Models[0].TurnCount == nil || *metadata.Models[0].TurnCount != 3 {
		t.Fatalf("model turn counts = %#v", metadata.Models)
	}
	kinds := turnKinds(view)
	want := map[int]TurnKind{0: TurnKindHumanPrompt, 1: TurnKindAssistant, 2: TurnKindLocalCommand, 4: TurnKindCompactSummary, 5: TurnKindCommandOutput, 6: TurnKindHumanPrompt, 7: TurnKindAssistant, 9: TurnKindCompactSummary, 10: TurnKindAssistant}
	for index, kind := range want {
		if kinds[index] != kind {
			t.Errorf("record %d kind = %q, want %q", index, kinds[index], kind)
		}
	}
	for _, boundary := range []int{3, 8} {
		if _, visible := kinds[boundary]; visible {
			t.Errorf("the boundary at record %d produced a turn", boundary)
		}
	}
	// show --normalized prints the turns as they are.
	for _, turn := range view.Turns {
		if turn.RecordIndex != 4 {
			continue
		}
		encoded, _ := json.Marshal(turn)
		if !bytes.Contains(encoded, []byte(`"kind":"compact_summary"`)) || !strings.Contains(turn.Text, "Summary: the widget parser was refactored") {
			t.Fatalf("summary turn = %s", encoded)
		}
	}
}

func claudeBundle(filterVersion string, records []map[string]any) SourceBundle {
	return SourceBundle{
		SchemaVersion: SourceSchemaVersion, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p",
		Capture:       SourceCapture{Harness: Harness{Name: "claude"}, AdapterName: "claude", AdapterVersion: adapterVersion, SourceFormat: "claude-jsonl", FilterVersion: filterVersion, CapturedAt: time.Date(2026, 9, 22, 13, 0, 0, 0, time.UTC)},
		NativeRecords: records,
	}
}

func metadataOf(t *testing.T, bundle SourceBundle) Metadata {
	t.Helper()
	at := bundle.Capture.CapturedAt
	metadata, err := BuildMetadata(bundle, "m", at, at, SourceReference{Key: "sessions/claude/a/source." + strings.Repeat("a", 64) + ".jsonl.gz", SHA256: strings.Repeat("a", 64)}, ParserInfo{})
	if err != nil {
		t.Fatalf("filter %s bundle does not parse: %v", bundle.Capture.FilterVersion, err)
	}
	return metadata
}

// A bundle captured before filter 5 carries neither the flags nor the boundary
// records. It keeps parser 0.7's counts (the summary still reads as a prompt,
// since nothing in it says otherwise) and reports compactions as unknown
// rather than zero.
func TestParserV08KeepsEarlierFilterCounts(t *testing.T) {
	_, records := filteredRecords(t, ClaudeAdapter{}, string(fixture(t, "claude-compaction.jsonl")))
	var withoutFlags []map[string]any
	for _, record := range records {
		if isCompactBoundary(record) {
			continue
		}
		delete(record, "isCompactSummary")
		delete(record, "isVisibleInTranscriptOnly")
		withoutFlags = append(withoutFlags, record)
	}
	for _, version := range []string{"2", "3", "4"} {
		metadata := metadataOf(t, claudeBundle(version, withoutFlags))
		countIs(t, "filter "+version+" turns", metadata.Counts.Turns, 4)
		countIs(t, "filter "+version+" messages", metadata.Counts.Messages, 7)
		if metadata.Counts.Compactions != nil {
			t.Errorf("filter %s reported compactions = %d; it cannot observe them", version, *metadata.Counts.Compactions)
		}
	}
	// A Codex session never reports them either.
	codex := claudeBundle(FilterVersion, []map[string]any{{"type": "message", "role": "user", "content": "go"}})
	codex.Capture.Harness.Name, codex.Capture.AdapterName, codex.Capture.SourceFormat = "codex", "codex", "codex-jsonl"
	if metadata := metadataOf(t, codex); metadata.Counts.Compactions != nil {
		t.Fatalf("a Codex bundle reported compactions")
	}
	// A filter 5 Claude session that never compacted reports zero.
	countIs(t, "compactions without any", metadataOf(t, claudeBundle(FilterVersion, []map[string]any{{"type": "user", "message": map[string]any{"role": "user", "content": "go"}}})).Counts.Compactions, 0)
}

// Each compaction writes a boundary and a summary; it is counted once. When no
// boundary was retained the summaries stand in, and a subagent's compaction
// inlined in its parent's transcript is the child's, not the parent's.
func TestParserV08CountsEachCompactionOnce(t *testing.T) {
	summary := func(id string, sidechain bool) map[string]any {
		return map[string]any{"type": "user", "uuid": id, "isCompactSummary": true, "isSidechain": sidechain, "message": map[string]any{"role": "user", "content": "summary"}}
	}
	boundary := func(id string, sidechain bool) map[string]any {
		return map[string]any{"type": "system", "subtype": "compact_boundary", "uuid": id, "isSidechain": sidechain}
	}
	countIs(t, "summaries only", metadataOf(t, claudeBundle(FilterVersion, []map[string]any{summary("s1", false), summary("s2", false)})).Counts.Compactions, 2)
	countIs(t, "boundary and summary", metadataOf(t, claudeBundle(FilterVersion, []map[string]any{boundary("b1", false), summary("s1", false)})).Counts.Compactions, 1)
	countIs(t, "inlined subagent", metadataOf(t, claudeBundle(FilterVersion, []map[string]any{boundary("b1", false), summary("s1", false), boundary("b2", true), summary("s2", true)})).Counts.Compactions, 1)
}

// /compact followed straight by the summary and then an assistant record (an
// auto-continued turn) is still not a prompt: the summary ends the scan for an
// answer, since the assistant cannot be answering a command the summary has
// summarized away.
func TestParserV08CompactCommandIsNeverPromotedAcrossTheSummary(t *testing.T) {
	bundle := claudeBundle(FilterVersion, []map[string]any{
		{"type": "user", "message": map[string]any{"role": "user", "content": "<command-name>/compact</command-name>"}},
		{"type": "user", "isCompactSummary": true, "message": map[string]any{"role": "user", "content": "summary"}},
		{"type": "assistant", "message": map[string]any{"id": "m1", "role": "assistant", "content": "continuing"}},
	})
	view, err := ParseNormalized(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if kinds := turnKinds(view); kinds[0] != TurnKindLocalCommand || kinds[1] != TurnKindCompactSummary {
		t.Fatalf("kinds = %#v", kinds)
	}
	countIs(t, "turns", metadataOf(t, bundle).Counts.Turns, 0)
}
