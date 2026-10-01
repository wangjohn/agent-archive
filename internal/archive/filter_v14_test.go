package archive

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// subagentTranscriptLines is a Claude Code subagent transcript of one prompt
// and one reply.
var subagentTranscriptLines = []string{
	`{"type":"user","uuid":"u1","sessionId":"native-parent","agentId":"agent-synthetic1","isSidechain":true,"timestamp":"2026-09-30T11:00:00Z","message":{"role":"user","content":"List the retention tests"}}`,
	`{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"native-parent","agentId":"agent-synthetic1","isSidechain":true,"timestamp":"2026-09-30T11:00:08Z","message":{"role":"assistant","content":[{"type":"text","text":"Two."}]}}`,
}

// filterSubagent filters subagentTranscriptLines with the contents of a
// .meta.json, and returns the retained records.
func filterSubagent(t *testing.T, meta string) (FilteredTranscript, []map[string]any) {
	t.Helper()
	filtered, err := (ClaudeAdapter{}).FilterSubagentJSONL(strings.NewReader(strings.Join(subagentTranscriptLines, "\n")+"\n"), []byte(meta))
	if err != nil {
		t.Fatal(err)
	}
	return filtered, decodeRecords(t, filtered)
}

// The description is kept as the first record, with the type and nothing
// else, and is redacted as prompt text is. Everything else in the file stays
// out without a gap: the file is not part of the transcript.
func TestFilterV14KeepsASubagentDescriptionAndRedactsIt(t *testing.T) {
	t.Parallel()
	meta := fixture(t, "claude-subagent.meta.json")
	filtered, err := (ClaudeAdapter{}).FilterSubagentJSONL(bytes.NewReader(fixture(t, "claude-subagent.jsonl")), meta)
	if err != nil {
		t.Fatal(err)
	}
	records := decodeRecords(t, filtered)
	if len(records) != 3 || records[0]["type"] != "subagent-meta" {
		t.Fatalf("records = %#v, want the subagent-meta record first, then the two of the transcript", records)
	}
	if got := strings.Join(sortedKeys(records[0]), " "); got != "description type" {
		t.Fatalf("subagent-meta keys = %q", got)
	}
	if got, want := records[0]["description"], "Find the retention tests token=[REDACTED]"; got != want {
		t.Fatalf("description = %q, want %q", got, want)
	}
	encoded := string(bytes.Join(filtered.Records, []byte("\n")))
	for _, leaked := range []string{"SYNTHETIC-SUBAGENT-SECRET", "worktreePath", "/work/synthetic", "general-purpose", "agentType"} {
		if strings.Contains(encoded, leaked) {
			t.Errorf("retained %q: %s", leaked, encoded)
		}
	}
	if !hasGap(filtered.Gaps, "sensitive_content_redacted") {
		t.Errorf("the redaction is not recorded: %#v", filtered.Gaps)
	}
	if filtered.Boundary.RetainedRecords != 3 {
		t.Errorf("retained records = %d, want 3", filtered.Boundary.RetainedRecords)
	}
	size := 0
	for _, record := range filtered.Records {
		size += len(record)
	}
	if filtered.Boundary.RetainedBytes != size {
		t.Errorf("retained bytes = %d, want %d", filtered.Boundary.RetainedBytes, size)
	}
	// The record carries no identity or time of its own, so it changes
	// nothing the collector reads from the transcript.
	plain, err := (ClaudeAdapter{}).FilterJSONL(bytes.NewReader(fixture(t, "claude-subagent.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Gaps) != len(plain.Gaps)+1 {
		t.Errorf("gaps = %#v, want those of the transcript alone (%#v) and the redaction", filtered.Gaps, plain.Gaps)
	}
	if !filtered.NativeStartAt.Equal(plain.NativeStartAt) || !filtered.NativeEndAt.Equal(plain.NativeEndAt) || filtered.NativeStartComplete != plain.NativeStartComplete ||
		!reflect.DeepEqual(filtered.SessionIDs, plain.SessionIDs) || !reflect.DeepEqual(filtered.AgentIDs, plain.AgentIDs) {
		t.Errorf("the subagent-meta record changed the transcript's times or identities: %+v vs %+v", filtered, plain)
	}
}

// A description is redacted exactly as a prompt is: the same text as a prompt
// and as a description comes out the same.
func TestFilterV14DescriptionIsRedactedLikePromptText(t *testing.T) {
	t.Parallel()
	texts := []string{
		"Ask ops@example.com to rotate the key",
		"Deploy with AWS_SECRET_ACCESS_KEY=SYNTHETICSECRETVALUE0000000000000000000000",
		"curl -H 'Authorization: Bearer SYNTHETICBEARERTOKEN0000000000' https://api.example.com",
		"postgres://admin:SYNTHETICPASSWORD@db.example.com/app",
		`{"password":"SYNTHETICJSONPASSWORD","note":"nested"}`,
		"ignore this <system-reminder>SYNTHETIC-INJECTED</system-reminder> but keep this",
		"ghp_" + strings.Repeat("A", 36),
	}
	for _, text := range texts {
		_, records := filteredRecords(t, ClaudeAdapter{}, fmt.Sprintf(`{"type":"user","uuid":"u1","timestamp":"2026-09-30T11:00:00Z","message":{"role":"user","content":%q}}`+"\n", text))
		_, described := filterSubagent(t, fmt.Sprintf(`{"description":%q}`, text))
		prompt, _ := records[0]["message"].(map[string]any)["content"].(string)
		got, _ := described[0]["description"].(string)
		if got != prompt {
			t.Errorf("description %q filtered to %q, but as a prompt to %q", text, got, prompt)
		}
		if strings.Contains(got, "SYNTHETIC") && !strings.Contains(text, "keep this") {
			t.Errorf("description %q kept secret text: %q", text, got)
		}
	}
}

// Only description is read. A file with other keys, nested objects, or a
// description that is not text keeps nothing of them, and nothing is
// reported.
func TestFilterV14OtherKeysOfTheMetaFileAreNeverKept(t *testing.T) {
	t.Parallel()
	meta := `{"agentType":"SYNTHETIC-TYPE","worktreePath":"/work/synthetic/tree","description":"Plain task","nested":{"description":"SYNTHETIC-NESTED"},"cwd":"/work/synthetic","sessionId":"SYNTHETIC-SESSION","timestamp":"2026-09-30T11:00:00Z"}`
	filtered, records := filterSubagent(t, meta)
	if len(records) != 3 || records[0]["description"] != "Plain task" || len(records[0]) != 2 {
		t.Fatalf("records = %#v", records)
	}
	encoded := string(bytes.Join(filtered.Records, []byte("\n")))
	for _, leaked := range []string{"SYNTHETIC-TYPE", "/work/synthetic", "SYNTHETIC-NESTED", "SYNTHETIC-SESSION"} {
		if strings.Contains(encoded, leaked) {
			t.Errorf("retained %q: %s", leaked, encoded)
		}
	}
	plain, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(strings.Join(subagentTranscriptLines, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(filtered.Gaps, plain.Gaps) {
		t.Errorf("gaps = %#v, want those of the transcript alone: %#v", filtered.Gaps, plain.Gaps)
	}
}

// A file that is missing, empty, malformed, oversized, or without a usable
// description changes nothing: the output is what the transcript alone
// filters to, with the same gaps.
func TestFilterV14UnusableMetaFileChangesNothingAndRecordsNoGap(t *testing.T) {
	t.Parallel()
	plain, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(strings.Join(subagentTranscriptLines, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"missing":           "",
		"empty object":      `{}`,
		"not JSON":          `{"description": "cut off`,
		"array":             `["description"]`,
		"null":              `null`,
		"no description":    `{"agentType":"general-purpose"}`,
		"blank":             `{"description":"   \n\t"}`,
		"empty":             `{"description":""}`,
		"number":            `{"description":7}`,
		"object":            `{"description":{"text":"SYNTHETIC-OBJECT"}}`,
		"list":              `{"description":["SYNTHETIC-LIST"]}`,
		"null description":  `{"description":null}`,
		"oversized file":    `{"description":"Plain task","pad":"` + strings.Repeat("x", MaxSubagentMetaBytes) + `"}`,
		"trailing garbage":  `{"description":"Plain task"} {"description":"two"}`,
		"binary":            "\x00\x01\x02",
		"byte order mark":   "\ufeff{\"description\":\"Plain task\"}",
		"whitespace only":   " \n ",
		"redaction removes": `{"description":"<system-reminder>SYNTHETIC-ONLY-INJECTED</system-reminder>"}`,
	}
	for name, meta := range cases {
		filtered, err := (ClaudeAdapter{}).FilterSubagentJSONL(strings.NewReader(strings.Join(subagentTranscriptLines, "\n")+"\n"), []byte(meta))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "redaction removes" {
			// Redaction takes the whole text: the record is dropped, as a
			// prompt with nothing left is, and the gap says so without
			// content.
			if len(filtered.Records) != len(plain.Records) {
				t.Errorf("%s: %d records, want the transcript's %d", name, len(filtered.Records), len(plain.Records))
			}
			continue
		}
		if !reflect.DeepEqual(filtered, plain) {
			t.Errorf("%s: filtered differently from the transcript alone:\n got %+v\nwant %+v", name, filtered, plain)
		}
	}
}

// A description is cut to maxSubagentDescriptionBytes on a character boundary
// and the cut is recorded. A secret that straddles the cut is redacted before
// it, so no part of it is left.
func TestFilterV14LongDescriptionIsCappedAfterRedaction(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", 2000)
	filtered, records := filterSubagent(t, fmt.Sprintf(`{"description":%q}`, long))
	description, _ := records[0]["description"].(string)
	if len(description) == 0 || len(description) > maxSubagentDescriptionBytes || !utf8.ValidString(description) {
		t.Fatalf("description is %d bytes (valid UTF-8: %v), want at most %d", len(description), utf8.ValidString(description), maxSubagentDescriptionBytes)
	}
	if !hasGap(filtered.Gaps, "content_truncated") {
		t.Errorf("the cut is not recorded: %#v", filtered.Gaps)
	}
	for offset := maxSubagentDescriptionBytes - 24; offset <= maxSubagentDescriptionBytes+8; offset += 4 {
		text := strings.Repeat("a ", offset/2) + "password=SYNTHETICSECRETVALUE123"
		_, records := filterSubagent(t, fmt.Sprintf(`{"description":%q}`, text))
		got, _ := records[0]["description"].(string)
		if strings.Contains(got, "SYNTH") || len(got) > maxSubagentDescriptionBytes {
			t.Errorf("offset %d: description = %q (%d bytes)", offset, got, len(got))
		}
	}
}

// What the filter wrote filters again to the same bytes (the collector filters
// a retained snapshot again when the filter changes), and a transcript holds
// one subagent-meta record: the one handed in wins over one in the transcript,
// and a second in the transcript is dropped with a gap.
func TestFilterV14SubagentMetaRefiltersUnchangedAndStaysSingle(t *testing.T) {
	t.Parallel()
	filtered, _ := filterSubagent(t, `{"description":"Find the retention tests"}`)
	again, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(string(bytes.Join(filtered.Records, []byte("\n"))) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Records, filtered.Records) || !reflect.DeepEqual(again.Boundary, filtered.Boundary) {
		t.Fatalf("filtered again: %q, want %q", again.Records, filtered.Records)
	}

	crowded := strings.Join(append([]string{
		`{"type":"subagent-meta","description":"From the transcript","sessionId":"s","uuid":"x"}`,
		`{"type":"subagent-meta","description":"Second"}`,
	}, subagentTranscriptLines...), "\n") + "\n"
	other, err := (ClaudeAdapter{}).FilterSubagentJSONL(strings.NewReader(crowded), []byte(`{"description":"From the file"}`))
	if err != nil {
		t.Fatal(err)
	}
	kept := claudeRecordsOfType(decodeRecords(t, other), "subagent-meta")
	if len(kept) != 1 || kept[0]["description"] != "From the file" || len(other.Records) != 3 {
		t.Fatalf("records = %q, want the file's description once, first", other.Records)
	}
	if !hasGap(other.Gaps, "unsupported_value_omitted") {
		t.Errorf("the dropped records are not reported: %#v", other.Gaps)
	}

	// With nothing handed in, the first one in the transcript stays, rebuilt
	// from the description alone, and the second is dropped.
	own, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(crowded))
	if err != nil {
		t.Fatal(err)
	}
	kept = claudeRecordsOfType(decodeRecords(t, own), "subagent-meta")
	if len(kept) != 1 || kept[0]["description"] != "From the transcript" || len(kept[0]) != 2 {
		t.Fatalf("records = %q", own.Records)
	}
	if !hasGap(own.Gaps, "unknown_field_omitted") || !hasGap(own.Gaps, "unsupported_value_omitted") {
		t.Errorf("gaps = %#v, want the dropped keys and the dropped second record", own.Gaps)
	}
}

// A subagent-meta record in a transcript with no usable description is
// dropped with a gap, as a custom-title with no title is.
func TestFilterV14SubagentMetaWithoutADescriptionIsDropped(t *testing.T) {
	t.Parallel()
	for _, line := range []string{`{"type":"subagent-meta"}`, `{"type":"subagent-meta","description":""}`, `{"type":"subagent-meta","description":{"a":"SYNTHETIC"}}`} {
		filtered, records := filterClaudeLines(t, append([]string{line}, subagentTranscriptLines...)...)
		if len(claudeRecordsOfType(records, "subagent-meta")) != 0 || len(records) != 2 || strings.Contains(string(bytes.Join(filtered.Records, nil)), "SYNTHETIC") {
			t.Errorf("%s: records = %#v", line, records)
		}
		if !hasGap(filtered.Gaps, "unsupported_value_omitted") {
			t.Errorf("%s: no gap: %#v", line, filtered.Gaps)
		}
	}
}

// The record neither fills an empty transcript nor makes an unrecognized one
// acceptable: the collector waits for a subagent's first record, and refuses
// a file with none the filter knows. A description it does not write leaves
// no gap either: the output is the transcript's alone.
func TestFilterV14MetaFileAloneNeverMakesATranscript(t *testing.T) {
	t.Parallel()
	for name, meta := range map[string][]byte{
		"redacted text": []byte(`{"description":"Find the retention tests password=SYNTHETICSUBAGENTPW"}`),
		// JSON text is filtered as JSON, as a prompt's is, and the names of
		// the keys it drops are held with the record.
		"JSON text": []byte(`{"description":"{\"usage\":{\"SYNTHETIC-KEY-NAME\":\"x\"}}"}`),
	} {
		empty, err := (ClaudeAdapter{}).FilterSubagentJSONL(strings.NewReader(""), meta)
		if err != nil || len(empty.Records) != 0 || empty.Boundary.RetainedRecords != 0 || len(empty.Gaps) != 0 {
			t.Fatalf("%s: empty transcript: %+v, %v", name, empty, err)
		}
		if _, err := (ClaudeAdapter{}).FilterSubagentJSONL(strings.NewReader(`{"type":"nothing-we-know"}`+"\n"), meta); !errors.Is(err, ErrUnsafeSourceFormat) {
			t.Fatalf("%s: unrecognized transcript: err = %v, want ErrUnsafeSourceFormat", name, err)
		}
		// Records that are all dropped leave no records to put it before, and
		// the description's redaction is not reported for a record not kept.
		untitled := `{"type":"custom-title","sessionId":"native-parent","timestamp":"2026-09-30T11:00:00Z"}` + "\n"
		plain, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(untitled))
		if err != nil {
			t.Fatal(err)
		}
		dropped, err := (ClaudeAdapter{}).FilterSubagentJSONL(strings.NewReader(untitled), meta)
		if err != nil || len(dropped.Records) != 0 || !reflect.DeepEqual(dropped, plain) {
			t.Fatalf("%s: all records dropped: %+v, want %+v (err %v)", name, dropped, plain, err)
		}
	}
	// Written, the record brings its omitted key names with it.
	filtered, _ := filterSubagent(t, `{"description":"{\"usage\":{\"SYNTHETIC-KEY-NAME\":\"x\"}}"}`)
	if !hasGapDetail(filtered.Gaps, "unknown_field_omitted", "omitted keys: SYNTHETIC-KEY-NAME, agentId") {
		t.Errorf("the omitted key name is not reported with the record: %#v", filtered.Gaps)
	}
}

// Only Claude Code's filter takes the file: the other adapters' records of
// the same name are unknown, and their output has no such record.
func TestFilterV14OtherAdaptersKnowNoSubagentMeta(t *testing.T) {
	t.Parallel()
	line := `{"type":"subagent-meta","description":"Find the retention tests"}` + "\n"
	for _, adapter := range []Adapter{CodexAdapter{}, CursorAdapter{}} {
		filtered, err := adapter.FilterJSONL(strings.NewReader(line))
		if err == nil && len(filtered.Records) != 0 {
			t.Errorf("%s kept %q", adapter.Name(), filtered.Records)
		}
	}
}

func TestSubagentMetaPath(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]string{
		"/h/.claude/projects/p/s/subagents/agent-a1b2.jsonl": "/h/.claude/projects/p/s/subagents/agent-a1b2.meta.json",
		"agent-x.jsonl":                  "agent-x.meta.json",
		"/h/p/agent-with-dashes-1.jsonl": "/h/p/agent-with-dashes-1.meta.json",
	} {
		if got, ok := SubagentMetaPath(path); !ok || got != want {
			t.Errorf("SubagentMetaPath(%q) = %q, %v; want %q", path, got, ok, want)
		}
	}
	for _, path := range []string{"", "/h/p/session.jsonl", "/h/p/agent-.jsonl", "/h/p/agent-x.json", "/h/p/agent-x.jsonl.bak", "/h/p/xagent-x.jsonl", "/h/p/agent-x.meta.json", "/h/p/agent-x/"} {
		if got, ok := SubagentMetaPath(path); ok {
			t.Errorf("SubagentMetaPath(%q) = %q, want none", path, got)
		}
	}
}

func TestWithoutSubagentMeta(t *testing.T) {
	t.Parallel()
	meta := map[string]any{"type": "subagent-meta", "description": "d"}
	user := map[string]any{"type": "user"}
	if got := WithoutSubagentMeta("claude-jsonl", []map[string]any{meta, user}); len(got) != 1 || got[0]["type"] != "user" {
		t.Errorf("claude: %#v", got)
	}
	if got := WithoutSubagentMeta("claude-jsonl", []map[string]any{user, meta}); len(got) != 2 {
		t.Errorf("a record after the first is evidence: %#v", got)
	}
	if got := WithoutSubagentMeta("codex-jsonl", []map[string]any{meta, user}); len(got) != 2 {
		t.Errorf("another format: %#v", got)
	}
	if got := WithoutSubagentMeta("claude-jsonl", nil); len(got) != 0 {
		t.Errorf("none: %#v", got)
	}
}

// subagentBundle is a subagent transcript filtered with meta, as a bundle.
func subagentBundle(t *testing.T, meta string, extra ...string) SourceBundle {
	t.Helper()
	lines := append(append([]string{}, subagentTranscriptLines...), extra...)
	filtered, err := (ClaudeAdapter{}).FilterSubagentJSONL(strings.NewReader(strings.Join(lines, "\n")+"\n"), []byte(meta))
	if err != nil {
		t.Fatal(err)
	}
	return parserTestBundle(t, "claude", ClaudeAdapter{}, filtered)
}

// Parser 0.18.0 names a subagent by its description, collapsed like a title;
// a transcript without one has no name; and a custom-title, which comes after
// it, wins as a later name always does. Nothing else of the metadata changes.
func TestSubagentNameComesFromTheDescription(t *testing.T) {
	t.Parallel()
	named := subagentBundle(t, `{"description":"  Find the\n  retention tests  "}`)
	// A subagent's own prompts are sidechain records, which are not the
	// person's, so it has a name and no title.
	labels, _ := SessionLabels(named)
	if labels.Name != "Find the retention tests" || labels.Title != "" {
		t.Fatalf("labels = %#v", labels)
	}
	long := subagentBundle(t, fmt.Sprintf(`{"description":%q}`, strings.Repeat("é", sessionTitleLimit+30)))
	longLabels, _ := SessionLabels(long)
	if want := strings.Repeat("é", sessionTitleLimit) + "…"; longLabels.Name != want {
		t.Fatalf("name = %q, want %d runes and an ellipsis", longLabels.Name, sessionTitleLimit)
	}
	bare := subagentBundle(t, "")
	if bareLabels, _ := SessionLabels(bare); bareLabels.Name != "" {
		t.Fatalf("name = %q, want none", bareLabels.Name)
	}
	renamed := subagentBundle(t, `{"description":"Find the retention tests"}`, `{"type":"custom-title","customTitle":"Chosen name","sessionId":"native-parent"}`)
	if renamedLabels, _ := SessionLabels(renamed); renamedLabels.Name != "Chosen name" {
		t.Fatalf("name = %q, want the custom-title", renamedLabels.Name)
	}

	withName, without := parserTestMetadata(t, named), parserTestMetadata(t, bare)
	if withName.Name != "Find the retention tests" || without.Name != "" {
		t.Fatalf("metadata names = %q, %q", withName.Name, without.Name)
	}
	withName.Name = ""
	withName.CaptureGaps, without.CaptureGaps = nil, nil
	a, _ := json.Marshal(withName)
	b, _ := json.Marshal(without)
	if string(a) != string(b) {
		t.Errorf("the description changed more than the name:\n%s\n%s", a, b)
	}
}
