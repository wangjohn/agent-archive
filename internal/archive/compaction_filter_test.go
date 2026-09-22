package archive

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

func filteredRecords(t *testing.T, adapter Adapter, jsonl string) (FilteredTranscript, []map[string]any) {
	t.Helper()
	filtered, err := adapter.FilterJSONL(strings.NewReader(jsonl))
	if err != nil {
		t.Fatal(err)
	}
	records := make([]map[string]any, 0, len(filtered.Records))
	for _, raw := range filtered.Records {
		var record map[string]any
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return filtered, records
}

func sortedKeys(record map[string]any) []string {
	keys := make([]string, 0, len(record))
	for key := range record {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Filter 5 keeps a compact_boundary system record's identity and nothing else
// of it: no text, no compaction metadata, no working directory.
func TestFilterV5KeepsOnlyTheCompactBoundaryIdentity(t *testing.T) {
	filtered, records := filteredRecords(t, ClaudeAdapter{}, string(fixture(t, "claude-compaction.jsonl")))
	var boundaries []map[string]any
	for _, record := range records {
		if record["type"] == "system" {
			boundaries = append(boundaries, record)
		}
	}
	if len(boundaries) != 2 {
		t.Fatalf("retained %d compact boundaries, want 2", len(boundaries))
	}
	want := "isSidechain logicalParentUuid parentUuid sessionId subtype timestamp type uuid"
	if got := strings.Join(sortedKeys(boundaries[0]), " "); got != want {
		t.Fatalf("boundary keys = %q, want %q", got, want)
	}
	if boundaries[0]["subtype"] != "compact_boundary" || boundaries[0]["logicalParentUuid"] != "k3" || boundaries[0]["parentUuid"] != nil || boundaries[0]["timestamp"] != "2026-09-22T12:01:02Z" {
		t.Fatalf("boundary = %#v", boundaries[0])
	}
	encoded, _ := json.Marshal(boundaries)
	for _, leaked := range []string{"synthetic-boundary-secret", "Conversation compacted", "preTokens", "trigger", "/work/widget", "external"} {
		if bytes.Contains(encoded, []byte(leaked)) {
			t.Fatalf("boundary retained %q: %s", leaked, encoded)
		}
	}
	omitted := gapDetail(filtered.Gaps, "unknown_field_omitted")
	for _, name := range []string{"compactMetadata", "content", "cwd", "level", "userType"} {
		if !strings.Contains(omitted, name) {
			t.Errorf("omitted key %q is not reported: %q", name, omitted)
		}
	}
}

// The compaction summary keeps its text (model output, useful for handoff)
// and the two flags, as booleans.
func TestFilterV5KeepsTheCompactSummaryTextAndFlags(t *testing.T) {
	_, records := filteredRecords(t, ClaudeAdapter{}, string(fixture(t, "claude-compaction.jsonl")))
	summary := records[4]
	if summary["isCompactSummary"] != true || summary["isVisibleInTranscriptOnly"] != true {
		t.Fatalf("flags not retained: %#v", summary)
	}
	message, _ := summary["message"].(map[string]any)
	if text, _ := message["content"].(string); !strings.Contains(text, "Summary: the widget parser was refactored") {
		t.Fatalf("summary text not retained: %#v", summary)
	}
}

// The flags are admitted only as booleans, like isMeta: prose under those
// names stays omitted, and its key name is reported.
func TestFilterV5AdmitsCompactionFlagsOnlyAsBooleans(t *testing.T) {
	line := `{"type":"user","uuid":"x1","timestamp":"2026-09-22T12:00:00Z","isCompactSummary":"yes, and here is some prose","isVisibleInTranscriptOnly":1,"message":{"role":"user","content":"hello"}}` + "\n"
	filtered, records := filteredRecords(t, ClaudeAdapter{}, line)
	if _, kept := records[0]["isCompactSummary"]; kept {
		t.Fatalf("a non-boolean isCompactSummary was retained: %#v", records[0])
	}
	if _, kept := records[0]["isVisibleInTranscriptOnly"]; kept {
		t.Fatalf("a non-boolean isVisibleInTranscriptOnly was retained: %#v", records[0])
	}
	omitted := gapDetail(filtered.Gaps, "unknown_field_omitted")
	if !strings.Contains(omitted, "isCompactSummary") || !strings.Contains(omitted, "isVisibleInTranscriptOnly") {
		t.Fatalf("omitted flags not reported: %q", omitted)
	}
}

// Only the exact compact_boundary shape is admitted, only from Claude Code,
// and only with values that look like what it is: every other system record
// stays hidden whole, and a malformed id or timestamp is dropped.
func TestFilterV5AdmitsOnlyTheExactCompactBoundary(t *testing.T) {
	other := `{"type":"system","subtype":"informational","content":"hidden system text","uuid":"s1","timestamp":"2026-09-22T12:00:00Z"}` + "\n"
	malformed := `{"type":"system","subtype":"compact_boundary","uuid":"has spaces in it","sessionId":{"nested":"object"},"timestamp":"not a time","isSidechain":"no"}` + "\n"
	prompt := `{"type":"user","uuid":"u1","timestamp":"2026-09-22T12:00:00Z","message":{"role":"user","content":"hello"}}` + "\n"

	_, records := filteredRecords(t, ClaudeAdapter{}, other+malformed+prompt)
	if len(records) != 2 {
		t.Fatalf("records = %#v", records)
	}
	if encoded, _ := json.Marshal(records); bytes.Contains(encoded, []byte("hidden system text")) || bytes.Contains(encoded, []byte("informational")) {
		t.Fatalf("an ordinary system record was retained: %s", encoded)
	}
	if got := strings.Join(sortedKeys(records[0]), " "); got != "subtype type" {
		t.Fatalf("malformed boundary kept %q", got)
	}

	_, codexRecords := filteredRecords(t, CodexAdapter{}, `{"type":"system","subtype":"compact_boundary","uuid":"c1"}`+"\n"+`{"type":"session_meta","payload":{"id":"s"}}`+"\n")
	for _, record := range codexRecords {
		if record["type"] == "system" {
			t.Fatalf("a non-Claude transcript admitted a system record: %#v", codexRecords)
		}
	}
}
