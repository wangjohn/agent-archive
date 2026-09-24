package archive

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

var updateComposerGolden = flag.Bool("update-composer-golden", false, "rewrite testdata/cursor-composer/*.golden.json from the current filter")

// composerFixture is a synthetic chat on disk: the composerData value and the
// message rows in header order. A bubble without "value" is a missing row.
type composerFixture struct {
	Composer json.RawMessage `json:"composer"`
	Bubbles  []struct {
		ID    string          `json:"id"`
		Value json.RawMessage `json:"value"`
	} `json:"bubbles"`
}

func loadComposerFixture(t *testing.T, name string) CursorComposer {
	t.Helper()
	var fixture composerFixture
	if err := json.Unmarshal(readComposerTestdata(t, name), &fixture); err != nil {
		t.Fatal(err)
	}
	c := CursorComposer{Composer: fixture.Composer}
	for _, b := range fixture.Bubbles {
		c.Bubbles = append(c.Bubbles, CursorBubble{ID: b.ID, Value: b.Value})
	}
	return c
}

func readComposerTestdata(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "cursor-composer", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// composerGolden is everything FilterComposer returns, in a readable form.
type composerGolden struct {
	Error               string            `json:"error,omitempty"`
	Format              string            `json:"format,omitempty"`
	Records             []json.RawMessage `json:"records,omitempty"`
	Gaps                []CaptureGap      `json:"gaps,omitempty"`
	Boundary            *CaptureBoundary  `json:"boundary,omitempty"`
	SessionIDs          []string          `json:"session_ids,omitempty"`
	AgentIDs            []string          `json:"agent_ids,omitempty"`
	FirstEventAt        string            `json:"first_event_at,omitempty"`
	NativeStartAt       string            `json:"native_start_at,omitempty"`
	NativeEndAt         string            `json:"native_end_at,omitempty"`
	NativeStartComplete bool              `json:"native_start_complete,omitempty"`
}

func goldenTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.Format(time.RFC3339Nano)
}

func composerGoldenOf(t *testing.T, name string) []byte {
	t.Helper()
	filtered, err := (CursorAdapter{}).FilterComposer(loadComposerFixture(t, name))
	var golden composerGolden
	if err != nil {
		golden.Error = err.Error()
	} else {
		boundary := filtered.Boundary
		golden = composerGolden{
			Format: filtered.Format, Gaps: filtered.Gaps, Boundary: &boundary,
			SessionIDs: filtered.SessionIDs, AgentIDs: filtered.AgentIDs,
			FirstEventAt: goldenTime(filtered.FirstEventAt), NativeStartAt: goldenTime(filtered.NativeStartAt),
			NativeEndAt: goldenTime(filtered.NativeEndAt), NativeStartComplete: filtered.NativeStartComplete,
		}
		for _, record := range filtered.Records {
			golden.Records = append(golden.Records, json.RawMessage(record))
		}
	}
	encoded, err := json.MarshalIndent(golden, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(encoded, '\n')
}

// Every synthetic chat filters to exactly its golden output. Regenerate with
// -update-composer-golden, which is itself a statement that the filter's
// output changed on purpose (and needs a FilterVersion bump).
func TestCursorComposerGolden(t *testing.T) {
	names, err := filepath.Glob(filepath.Join("testdata", "cursor-composer", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var inputs []string
	for _, path := range names {
		if name := filepath.Base(path); !strings.HasSuffix(name, ".golden.json") {
			inputs = append(inputs, name)
		}
	}
	sort.Strings(inputs)
	if len(inputs) < 4 {
		t.Fatalf("fixtures = %v", inputs)
	}
	for _, name := range inputs {
		goldenName := strings.TrimSuffix(name, ".json") + ".golden.json"
		got := composerGoldenOf(t, name)
		if again := composerGoldenOf(t, name); !bytes.Equal(got, again) {
			t.Fatalf("%s: output is not deterministic", name)
		}
		path := filepath.Join("testdata", "cursor-composer", goldenName)
		if *updateComposerGolden {
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if want := readComposerTestdata(t, goldenName); !bytes.Equal(got, want) {
			t.Errorf("%s filtered differently:\n got: %s\nwant: %s", name, got, want)
		}
	}
}

func filterComposerFixture(t *testing.T, name string) (FilteredTranscript, []map[string]any) {
	t.Helper()
	filtered, err := (CursorAdapter{}).FilterComposer(loadComposerFixture(t, name))
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

// The allowlist keeps who said what, tool calls and results, model, tokens,
// and timestamps, and nothing of the context Cursor attached, the reasoning,
// secrets, injected instructions, or the chat's own settings.
func TestCursorComposerKeepsTheConversationAndDropsContext(t *testing.T) {
	filtered, records := filterComposerFixture(t, "chat.json")
	encoded := string(bytes.Join(filtered.Records, []byte("\n")))
	for _, leaked := range []string{
		"CONTEXT-SENTINEL", "THINKING-SENTINEL", "sk-SYNTHETIC", "hunter2", "SYNTHETIC-TOKEN-VALUE",
		"INJECTED-SENTINEL", "system-reminder", "UNKNOWN-TYPE-SENTINEL", "SYNTHETIC-HASH",
		"SYNTHETIC-ENCRYPTION-KEY", "SYNTHETIC-CHAT-TITLE", "/Users/synthetic", "synthetic-model-current",
		"TOOL-ADDITIONAL-SENTINEL", "TOOLRESULT-EXTRA-SENTINEL", "PROVIDER-SENTINEL", "costInCents", "richText",
	} {
		if strings.Contains(encoded, leaked) {
			t.Errorf("retained %q", leaked)
		}
	}
	gaps, _ := json.Marshal(filtered.Gaps)
	for _, leaked := range []string{"SENTINEL", "SYNTHETIC", "b3", "c0ffee00"} {
		if bytes.Contains(gaps, []byte(leaked)) {
			t.Errorf("a gap carries a value or an id %q: %s", leaked, gaps)
		}
	}

	// session, b1, b2, b4, b5, b7: b3 has no row and b6 has an unknown type.
	if len(records) != 6 {
		t.Fatalf("records = %d: %s", len(records), encoded)
	}
	if records[0]["type"] != "session" || records[0]["session_id"] != "c0ffee00-0000-4000-8000-000000000001" || records[0]["timestamp"] != "2026-09-21T14:13:20Z" {
		t.Fatalf("session record = %#v", records[0])
	}
	var ids []string
	for _, record := range records[1:] {
		ids = append(ids, firstString(record, "id"))
	}
	if got := strings.Join(ids, " "); got != "b1 b2 b4 b5 b7" {
		t.Fatalf("message order = %q", got)
	}
	user := records[1]
	if user["role"] != "user" || user["requestId"] != "req-1" || user["timestamp"] != "2026-09-21T14:13:21Z" {
		t.Fatalf("user record = %#v", user)
	}
	if _, kept := user["usage"]; kept {
		t.Errorf("an all-zero token count was retained: %#v", user)
	}
	if text := contentText(user["message"].(map[string]any)["content"]); !strings.Contains(text, "Fix the widget test.") || !strings.Contains(text, "[REDACTED]") {
		t.Errorf("user text = %q", text)
	}
	assistant := records[2]
	if assistant["role"] != "assistant" || assistant["model"] != "synthetic-model-a" || assistant["started_at_ms"] != 1790000002000.0 || assistant["completed_at_ms"] != 1790000002500.0 {
		t.Fatalf("assistant record = %#v", assistant)
	}
	if usage := assistant["usage"].(map[string]any); usage["input_tokens"] != 1200.0 || usage["output_tokens"] != 45.0 {
		t.Errorf("usage = %#v", usage)
	}
	content := assistant["message"].(map[string]any)["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("assistant content = %#v", content)
	}
	call, result := content[1].(map[string]any), content[2].(map[string]any)
	if call["type"] != "tool_use" || call["id"] != "call-1" || call["name"] != "read_file" {
		t.Fatalf("tool call = %#v", call)
	}
	// rawArgs wins over params, decoded into an object.
	if input := call["input"].(map[string]any); input["target_file"] != "internal/widget/widget_test.go" || input["limit"] != 80.0 {
		t.Fatalf("tool input = %#v", call["input"])
	}
	if result["type"] != "tool_result" || result["tool_use_id"] != "call-1" || result["status"] != "completed" || !strings.Contains(result["content"].(string), "package widget") {
		t.Fatalf("tool result = %#v", result)
	}
	if _, isError := result["is_error"]; isError {
		t.Errorf("a completed tool reported an error: %#v", result)
	}
	// A structured toolResults entry becomes one tool_result with its text.
	blobBubble := records[3]["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if blobBubble["tool_use_id"] != "call-0" || blobBubble["name"] != "run_terminal_cmd" || blobBubble["content"] != `{"exitCode":0,"output":"ok"}` {
		t.Fatalf("toolResults entry = %#v", blobBubble)
	}
	// The injected block is stripped and the rest of the prompt kept.
	if text := contentText(records[4]["message"].(map[string]any)["content"]); text != "Thanks, now run it." || records[4]["timestamp"] != "2026-09-21T14:13:25Z" {
		t.Errorf("second prompt = %#v", records[4])
	}
}

func TestCursorComposerReportsWhatItCouldNotKeep(t *testing.T) {
	filtered, _ := filterComposerFixture(t, "chat.json")
	want := map[string]string{
		"cursor_bubble_missing":           "1 of 7 messages have no message row",
		"cursor_blob_content_unavailable": "1 of 7 messages reference content blobs, which are not read",
		"cursor_message_type_unknown":     "1 of 7 messages have an unknown or inconsistent type",
		"hidden_instruction_omitted":      "",
		"sensitive_content_redacted":      "content redacted",
	}
	for code, detail := range want {
		if !hasGap(filtered.Gaps, code) {
			t.Errorf("gap %s missing: %#v", code, filtered.Gaps)
		} else if detail != "" && gapDetail(filtered.Gaps, code) != detail {
			t.Errorf("gap %s detail = %q, want %q", code, gapDetail(filtered.Gaps, code), detail)
		}
	}
	context := gapDetail(filtered.Gaps, "cursor_context_omitted")
	for _, name := range []string{"attachedCodeChunks", "codebaseContextChunks", "consoleLogs", "diffHistories", "images", "originalFileStates", "recentlyViewedFiles"} {
		if !strings.Contains(context, name) {
			t.Errorf("context field %s not reported: %q", name, context)
		}
	}
	// An empty context payload (b2's codebaseContextChunks, b1's lints) lost
	// nothing, so it is not named.
	if strings.Contains(context, "lints") {
		t.Errorf("an empty context field was reported: %q", context)
	}
	omitted := gapDetail(filtered.Gaps, "unknown_field_omitted")
	for _, name := range []string{"lastUpdatedAt", "modelConfig", "usageData", "workspaceIdentifier", "isAgentic", "additionalData", "modelCallId", "extra", "provider", "cacheTokens"} {
		if !strings.Contains(omitted, name) {
			t.Errorf("omitted key %s not reported: %q", name, omitted)
		}
	}
	// Consumed keys lose nothing and are not reported, and blobEncryptionKey
	// is not content.
	for _, name := range []string{"composerId", "fullConversationHeadersOnly", "bubbleId", "richText", "toolFormerData", "blobEncryptionKey"} {
		if strings.Contains(omitted, name+",") || strings.HasSuffix(omitted, name) {
			t.Errorf("consumed key %s reported as omitted: %q", name, omitted)
		}
	}
	for i := 1; i < len(filtered.Gaps); i++ {
		if filtered.Gaps[i-1].Code > filtered.Gaps[i].Code {
			t.Fatalf("gaps are not ordered by code: %#v", filtered.Gaps)
		}
	}
}

// The chat starts at createdAt and ends when its last message completed, even
// though the last header is not the latest-updated field in the chat.
func TestCursorComposerTimestampsAndIdentity(t *testing.T) {
	filtered, _ := filterComposerFixture(t, "chat.json")
	start := time.UnixMilli(1790000000000).UTC()
	if !filtered.NativeStartAt.Equal(start) || !filtered.FirstEventAt.Equal(start) || !filtered.NativeStartComplete {
		t.Fatalf("start = %v first = %v complete = %v", filtered.NativeStartAt, filtered.FirstEventAt, filtered.NativeStartComplete)
	}
	if end := time.UnixMilli(1790000009000).UTC(); !filtered.NativeEndAt.Equal(end) {
		t.Fatalf("end = %v, want %v", filtered.NativeEndAt, end)
	}
	if len(filtered.SessionIDs) != 1 || filtered.SessionIDs[0] != "c0ffee00-0000-4000-8000-000000000001" || len(filtered.AgentIDs) != 0 {
		t.Fatalf("ids = %v %v", filtered.SessionIDs, filtered.AgentIDs)
	}
	if filtered.Format != "cursor-composer" {
		t.Fatalf("format = %q", filtered.Format)
	}
}

// An older chat with its messages inline is filtered like rows, without a
// createdAt its start comes from its messages and is not complete, and an
// entry in another shape is counted rather than guessed at.
func TestCursorComposerInlineConversation(t *testing.T) {
	filtered, records := filterComposerFixture(t, "inline-conversation.json")
	if len(records) != 3 || records[1]["id"] != "i1" || records[2]["id"] != "i2" || records[2]["role"] != "assistant" {
		t.Fatalf("records = %#v", records)
	}
	if filtered.NativeStartComplete || !filtered.NativeStartAt.IsZero() {
		t.Fatalf("start = %v complete = %v", filtered.NativeStartAt, filtered.NativeStartComplete)
	}
	if got := gapDetail(filtered.Gaps, "cursor_conversation_entry_unsupported"); got != "2 of 4 inline conversation entries are not in the message shape" {
		t.Fatalf("gap = %q (%#v)", got, filtered.Gaps)
	}
	encoded := string(bytes.Join(filtered.Records, []byte("\n")))
	if strings.Contains(encoded, "SENTINEL") {
		t.Fatalf("retained a sentinel: %s", encoded)
	}
}

// Fail closed: a format version this filter does not know, on the chat or on
// any message, refuses the whole chat as an unsafe format.
func TestCursorComposerRefusesUnknownVersions(t *testing.T) {
	for _, name := range []string{"unknown-composer-version.json", "unknown-bubble-version.json"} {
		_, err := (CursorAdapter{}).FilterComposer(loadComposerFixture(t, name))
		if !errors.Is(err, ErrUnsafeSourceFormat) || !IsFilterError(err) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	for _, composer := range []string{
		`{"composerId":"c","createdAt":1}`, // no _v
		`{"_v":"18","composerId":"c"}`,     // not a number
		`{"_v":18.5,"composerId":"c"}`,     // not an integer
		`{"_v":0,"composerId":"c"}`,        // not positive
	} {
		_, err := (CursorAdapter{}).FilterComposer(CursorComposer{Composer: json.RawMessage(composer)})
		if !errors.Is(err, ErrUnsafeSourceFormat) {
			t.Errorf("%s: err = %v", composer, err)
		}
	}
	// A message without _v, even in an inline conversation.
	inline := `{"_v":2,"composerId":"c","conversation":[{"bubbleId":"i1","type":1,"text":"x"}]}`
	if _, err := (CursorAdapter{}).FilterComposer(CursorComposer{Composer: json.RawMessage(inline)}); !errors.Is(err, ErrUnsafeSourceFormat) {
		t.Errorf("inline message without _v: err = %v", err)
	}
}

// Malformed input and a message list that does not match the headers are
// refused with a FilterError, never filtered partially.
func TestCursorComposerRefusesMalformedInput(t *testing.T) {
	headers := `{"_v":18,"composerId":"c","fullConversationHeadersOnly":[{"bubbleId":"b1","type":1}]}`
	cases := map[string]CursorComposer{
		"composer not JSON":   {Composer: json.RawMessage(`{"_v":18,`)},
		"composer not object": {Composer: json.RawMessage(`[1]`)},
		"no composer ID":      {Composer: json.RawMessage(`{"_v":18}`)},
		"bubble not JSON":     {Composer: json.RawMessage(headers), Bubbles: []CursorBubble{{ID: "b1", Value: json.RawMessage(`{"_v":3,`)}}},
		"fewer bubbles":       {Composer: json.RawMessage(headers)},
		"wrong bubble ID":     {Composer: json.RawMessage(headers), Bubbles: []CursorBubble{{ID: "b2", Value: json.RawMessage(`{"_v":3,"type":1}`)}}},
		"bubbles, no headers": {Composer: json.RawMessage(`{"_v":18,"composerId":"c"}`), Bubbles: []CursorBubble{{ID: "b1", Value: json.RawMessage(`{"_v":3,"type":1}`)}}},
	}
	for name, c := range cases {
		_, err := (CursorAdapter{}).FilterComposer(c)
		if !IsFilterError(err) {
			t.Errorf("%s: err = %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "b1") {
			t.Errorf("%s: error names a message: %v", name, err)
		}
	}
}

// A row that is JSON null is as missing as no row at all, and a header whose
// type disagrees with its row is counted, not attributed to either role.
func TestCursorComposerNullRowAndTypeMismatch(t *testing.T) {
	c := CursorComposer{
		Composer: json.RawMessage(`{"_v":18,"composerId":"c","createdAt":1790000000000,"fullConversationHeadersOnly":[{"bubbleId":"b1","type":1},{"bubbleId":"b2","type":1,"createdAt":1790000002000}]}`),
		Bubbles: []CursorBubble{
			{ID: "b1", Value: json.RawMessage(`{"_v":3,"bubbleId":"b1","type":2,"text":"MISMATCH-SENTINEL"}`)},
			{ID: "b2", Value: json.RawMessage(`null`)},
		},
	}
	filtered, err := (CursorAdapter{}).FilterComposer(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Records) != 1 || bytes.Contains(bytes.Join(filtered.Records, nil), []byte("SENTINEL")) {
		t.Fatalf("records = %s", bytes.Join(filtered.Records, []byte("\n")))
	}
	if gapDetail(filtered.Gaps, "cursor_bubble_missing") != "1 of 2 messages have no message row" || !hasGap(filtered.Gaps, "cursor_message_type_unknown") {
		t.Fatalf("gaps = %#v", filtered.Gaps)
	}
	// The missing message's header still dates the end of the chat.
	if end := time.UnixMilli(1790000002000).UTC(); !filtered.NativeEndAt.Equal(end) {
		t.Fatalf("end = %v", filtered.NativeEndAt)
	}
}

// A typed-input tool's text and a credential-named argument are dropped by
// the shared tool filter, exactly as in a JSONL transcript.
func TestCursorComposerToolArgumentsUseTheSharedDenyList(t *testing.T) {
	c := CursorComposer{
		Composer: json.RawMessage(`{"_v":18,"composerId":"c","fullConversationHeadersOnly":[{"bubbleId":"b1","type":2}]}`),
		Bubbles:  []CursorBubble{{ID: "b1", Value: json.RawMessage(`{"_v":3,"bubbleId":"b1","type":2,"toolFormerData":{"toolCallId":"t1","name":"mcp__browser__type","status":"error","params":{"selector":"#login","text":"TYPED-SENTINEL","apiToken":"ARG-SENTINEL"},"result":"failed"}}`)}},
	}
	filtered, err := (CursorAdapter{}).FilterComposer(c)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(bytes.Join(filtered.Records, []byte("\n")))
	if strings.Contains(encoded, "SENTINEL") || !strings.Contains(encoded, "#login") || !strings.Contains(encoded, `"is_error":true`) {
		t.Fatalf("records = %s", encoded)
	}
	if denied := gapDetail(filtered.Gaps, "sensitive_or_hidden_field_omitted"); !strings.Contains(denied, "apiToken") || !strings.Contains(denied, "text") {
		t.Fatalf("denied = %q", denied)
	}
}

// The records are the shape the normalized view already reads: prompts and
// assistant messages are turns, the tool call links to its result, and token
// counts are summed.
func TestCursorComposerRecordsParse(t *testing.T) {
	filtered, _ := filterComposerFixture(t, "chat.json")
	reg := registration()
	reg.Harness = Harness{Name: "cursor"}
	bundle, err := NewSourceBundle(reg, CursorAdapter{}, filtered, time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Capture.SourceFormat != "cursor-composer" || bundle.Capture.FilterVersion != "8" {
		t.Fatalf("capture = %#v", bundle.Capture)
	}
	view, err := ParseNormalized(bundle)
	if err != nil {
		t.Fatal(err)
	}
	var prompts, replies int
	for _, turn := range view.Turns {
		switch turn.Kind {
		case TurnKindHumanPrompt:
			prompts++
		case TurnKindAssistant:
			replies++
			if turn.Model != "synthetic-model-a" && turn.Model != "" {
				t.Errorf("turn model = %q", turn.Model)
			}
		}
	}
	if prompts != 2 || replies != 3 {
		t.Fatalf("prompts = %d replies = %d: %#v", prompts, replies, view.Turns)
	}
	if len(view.ToolCalls) != 1 || view.ToolCalls[0].Name != "read_file" || view.ToolCalls[0].CallID != "call-1" {
		t.Fatalf("tool calls = %#v", view.ToolCalls)
	}
	if len(view.ToolResults) != 2 {
		t.Fatalf("tool results = %#v", view.ToolResults)
	}
	if view.Tokens.Input == nil || *view.Tokens.Input != 2600 || view.Tokens.Output == nil || *view.Tokens.Output != 65 {
		t.Fatalf("tokens = %#v", view.Tokens)
	}
}
