package archive

import (
	"bytes"
	"strings"
	"testing"
)

// An isMeta record is harness-written instruction text (an expanded skill or
// slash command). Filter 4 keeps the record, its ids, its parent link and the
// flag, and drops its text with a hidden_instruction_omitted gap.
func TestFilterV4StripsMetaRecordTextButKeepsTheRecord(t *testing.T) {
	filtered, err := (ClaudeAdapter{}).FilterJSONL(bytes.NewReader(fixture(t, "claude-skill-command.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	records := decodeRecords(t, filtered)
	if len(records) != 3 {
		t.Fatalf("retained %d records, want 3", len(records))
	}
	meta := records[1]
	if meta["uuid"] != "k2" || meta["parentUuid"] != "k1" || meta["isMeta"] != true {
		t.Fatalf("meta record identity or flag lost: %#v", meta)
	}
	message := child(t, meta, "message")
	if _, kept := message["content"]; kept || message["role"] != "user" {
		t.Fatalf("meta record content retained: %#v", message)
	}
	joined := string(bytes.Join(filtered.Records, []byte("\n")))
	if strings.Contains(joined, "Synthetic expanded skill instructions") || strings.Contains(joined, "Base directory") {
		t.Fatal("expanded skill text survived filtering")
	}
	// The typed slash command itself is what the person sent, and stays.
	if command, _ := child(t, records[0], "message")["content"].(string); !strings.Contains(command, "<command-name>/review-pr</command-name>") {
		t.Fatalf("slash command text lost: %q", command)
	}
	found := false
	for _, gap := range filtered.Gaps {
		if gap.Code == "hidden_instruction_omitted" && gap.Detail == "meta record text omitted" {
			found = true
		}
	}
	if !found {
		t.Fatalf("meta stripping was not recorded: %#v", filtered.Gaps)
	}
}

func TestFilterV4StripsOnlyMetaRecords(t *testing.T) {
	filtered, err := (ClaudeAdapter{}).FilterJSONL(bytes.NewReader(fixture(t, "claude-local-command.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	records := decodeRecords(t, filtered)
	if len(records) != 5 {
		t.Fatalf("retained %d records, want 5", len(records))
	}
	if _, kept := child(t, records[1], "message")["content"]; kept || records[1]["isMeta"] != true {
		t.Fatalf("isMeta caveat kept its text: %#v", records[1])
	}
	for _, index := range []int{0, 2, 3} {
		if content, _ := child(t, records[index], "message")["content"].(string); content == "" {
			t.Fatalf("record %d without isMeta lost its content: %#v", index, records[index])
		}
	}
}

// Only text is stripped from a meta record; any other block is left to the
// ordinary rules, and a record whose isMeta is not true is untouched.
func TestFilterV4MetaStrippingKeepsNonTextBlocks(t *testing.T) {
	input := `{"type":"user","uuid":"m1","timestamp":"2026-09-22T10:00:00Z","isMeta":true,"message":{"role":"user","content":[{"type":"text","text":"expanded instructions"},{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}}` + "\n" +
		`{"type":"user","uuid":"m2","timestamp":"2026-09-22T10:00:01Z","isMeta":false,"message":{"role":"user","content":"typed by a person"}}`
	filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	records := decodeRecords(t, filtered)
	blocks, _ := child(t, records[0], "message")["content"].([]any)
	if len(blocks) != 1 || blocks[0].(map[string]any)["type"] != "tool_result" {
		t.Fatalf("meta record blocks = %#v", blocks)
	}
	if strings.Contains(string(filtered.Records[0]), "expanded instructions") {
		t.Fatal("meta text survived beside a non-text block")
	}
	if content, _ := child(t, records[1], "message")["content"].(string); content != "typed by a person" || records[1]["isMeta"] != false {
		t.Fatalf("non-meta record changed: %#v", records[1])
	}
}

// The isMeta allowlist entry admits only the boolean flag. A string or object
// under that key is prose filter 3 never retained, and filter 4 must not start
// retaining it anywhere the ordinary allowlist applies.
func TestFilterV4RetainsIsMetaOnlyAsABoolean(t *testing.T) {
	input := `{"type":"user","uuid":"m1","timestamp":"2026-09-22T10:00:00Z","isMeta":"prose under the flag name","message":{"role":"user","content":"hi","isMeta":{"note":"nested prose under the flag name"}}}` + "\n" +
		`{"type":"user","uuid":"m2","timestamp":"2026-09-22T10:00:01Z","isMeta":true,"message":{"role":"user","content":"expanded"}}`
	filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	joined := string(bytes.Join(filtered.Records, []byte("\n")))
	if strings.Contains(joined, "prose under the flag name") {
		t.Fatalf("a non-boolean isMeta was retained: %s", joined)
	}
	records := decodeRecords(t, filtered)
	if _, kept := records[0]["isMeta"]; kept {
		t.Fatalf("string isMeta kept: %#v", records[0])
	}
	if _, kept := child(t, records[0], "message")["isMeta"]; kept {
		t.Fatalf("object isMeta kept: %#v", records[0])
	}
	if records[1]["isMeta"] != true {
		t.Fatalf("boolean isMeta lost: %#v", records[1])
	}
}

// Text is stripped from an isMeta record at every depth: a tool result inside
// it keeps its identifiers but loses its nested content text, and a block of
// an unknown type loses its own text.
func TestFilterV4StripsNestedTextInMetaRecords(t *testing.T) {
	input := `{"type":"user","uuid":"m1","timestamp":"2026-09-22T10:00:00Z","isMeta":true,"message":{"role":"user","content":[{"type":"text","text":"expanded instructions"},{"type":"tool_result","tool_use_id":"toolu_1","is_error":false,"content":[{"type":"text","text":"nested tool text"},{"type":"text","text":"more nested text"}]},{"type":"tool_result","tool_use_id":"toolu_2","content":"string tool text"},{"type":"other","text":"loose text","name":"kept"}]}}`
	filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	joined := string(bytes.Join(filtered.Records, []byte("\n")))
	for _, text := range []string{"expanded instructions", "nested tool text", "more nested text", "string tool text", "loose text"} {
		if strings.Contains(joined, text) {
			t.Fatalf("meta record text %q survived: %s", text, joined)
		}
	}
	records := decodeRecords(t, filtered)
	blocks, _ := child(t, records[0], "message")["content"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("meta record blocks = %#v", blocks)
	}
	first := blocks[0].(map[string]any)
	if first["type"] != "tool_result" || first["tool_use_id"] != "toolu_1" || first["is_error"] != false {
		t.Fatalf("tool result lost its identifiers: %#v", first)
	}
	if _, kept := first["content"]; kept {
		t.Fatalf("tool result kept nested content: %#v", first)
	}
	if third := blocks[2].(map[string]any); third["name"] != "kept" {
		t.Fatalf("non-text field of an unknown block lost: %#v", third)
	}
}
