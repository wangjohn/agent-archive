package archive

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

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
		golden = composerGolden{Error: err.Error()}
	} else {
		boundary := filtered.Boundary
		var records []json.RawMessage
		for _, record := range filtered.Records {
			records = append(records, json.RawMessage(record))
		}
		golden = composerGolden{
			Format: filtered.Format, Gaps: filtered.Gaps, Boundary: &boundary,
			SessionIDs: filtered.SessionIDs, AgentIDs: filtered.AgentIDs,
			FirstEventAt: goldenTime(filtered.FirstEventAt), NativeStartAt: goldenTime(filtered.NativeStartAt),
			NativeEndAt: goldenTime(filtered.NativeEndAt), NativeStartComplete: filtered.NativeStartComplete,
			Records: records,
		}
	}
	encoded, err := json.MarshalIndent(golden, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(encoded, '\n')
}

// Every synthetic chat filters to exactly its golden output. Regenerate with
// -update, which is itself a statement that the filter's output changed on
// purpose (and needs a FilterVersion bump).
func TestCursorComposerGolden(t *testing.T) {
	t.Parallel()
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
	if len(inputs) < 3 {
		t.Fatalf("fixtures = %v", inputs)
	}
	for _, name := range inputs {
		goldenName := strings.TrimSuffix(name, ".json") + ".golden.json"
		got := composerGoldenOf(t, name)
		if again := composerGoldenOf(t, name); !bytes.Equal(got, again) {
			t.Fatalf("%s: output is not deterministic", name)
		}
		path := filepath.Join("testdata", "cursor-composer", goldenName)
		if golden.Update() {
			golden.Write(t, path, got)
			continue
		}
		if want := readComposerTestdata(t, goldenName); !bytes.Equal(got, want) {
			t.Errorf("%s filtered differently:\n got: %s\nwant: %s", name, got, want)
		}
	}
}

func filterComposerFixture(t *testing.T) (FilteredTranscript, []map[string]any) {
	t.Helper()
	filtered, err := (CursorAdapter{}).FilterComposer(loadComposerFixture(t, "chat.json"))
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
	t.Parallel()
	filtered, records := filterComposerFixture(t)
	encoded := string(bytes.Join(filtered.Records, []byte("\n")))
	for _, leaked := range []string{
		"CONTEXT-SENTINEL", "THINKING-SENTINEL", "sk-SYNTHETIC", "hunter2", "SYNTHETIC-TOKEN-VALUE",
		"INJECTED-SENTINEL", "system-reminder", "UNKNOWN-TYPE-SENTINEL", "SYNTHETIC-HASH",
		"SYNTHETIC-ENCRYPTION-KEY", "SYNTHETIC-CHAT-TITLE", "/Users/synthetic", "synthetic-model-current",
		"TOOL-ADDITIONAL-SENTINEL", "TOOLRESULT-EXTRA-SENTINEL", "PROVIDER-SENTINEL", "costInCents", "richText",
		"TOOLBINARY-SENTINEL", "USERDECISION-SENTINEL", "NESTED-SENTINEL", "ERROR-SECRET-SENTINEL", "RESULT-BESIDE-ERROR-SENTINEL",
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
	// A failed tool: rawArgs is cut off, so the arguments come from params,
	// without the argument that was itself JSON and without the empty options
	// object; the error, not the result, is the tool_result.
	failed := records[5]["message"].(map[string]any)["content"].([]any)
	if len(failed) != 3 {
		t.Fatalf("failed tool content = %#v", failed)
	}
	failedCall, failedResult := failed[1].(map[string]any), failed[2].(map[string]any)
	input, _ := failedCall["input"].(map[string]any)
	if failedCall["name"] != "run_terminal_command_v2" || input["command"] != "go test ./internal/widget/" || input["commandDescription"] != "Run the widget tests" {
		t.Fatalf("failed call = %#v", failedCall)
	}
	for _, dropped := range []string{"requestedSandboxPolicy", "options"} {
		if _, kept := input[dropped]; kept {
			t.Errorf("argument %s retained: %#v", dropped, input)
		}
	}
	if _, kept := input["parsingResult"].(map[string]any); !kept {
		t.Errorf("a plain object argument was dropped: %#v", input)
	}
	if failedResult["tool_use_id"] != "call-2" || failedResult["is_error"] != true || failedResult["content"] != "exit status 1: password=[REDACTED]" {
		t.Fatalf("failed result = %#v", failedResult)
	}
}

func TestCursorComposerReportsWhatItCouldNotKeep(t *testing.T) {
	t.Parallel()
	filtered, _ := filterComposerFixture(t)
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
	// Omitted key names say which level they were at.
	omitted := gapDetail(filtered.Gaps, "unknown_field_omitted")
	for _, name := range []string{
		"chat.lastUpdatedAt", "chat.modelConfig", "chat.usageData", "chat.workspaceIdentifier",
		"message.isAgentic", "tool.additionalData", "tool.modelCallId", "toolResult.extra", "tool.toolCallBinary",
		"tool.userDecision", "tool.result", "model.provider", "tokens.cacheTokens",
	} {
		if !strings.Contains(omitted, name) {
			t.Errorf("omitted key %s not reported: %q", name, omitted)
		}
	}
	// Consumed keys lose nothing and are not reported, and blobEncryptionKey
	// is not content.
	for _, name := range []string{"composerId", "fullConversationHeadersOnly", "bubbleId", "richText", "toolFormerData", "blobEncryptionKey", "rawArgs", "params", "error"} {
		if strings.Contains(omitted, "."+name) {
			t.Errorf("consumed key %s reported as omitted: %q", name, omitted)
		}
	}
	if got := gapDetail(filtered.Gaps, "cursor_tool_argument_omitted"); got != "omitted tool arguments: requestedSandboxPolicy" {
		t.Errorf("tool argument gap = %q", got)
	}
	// An empty argument object is dropped without a gap.
	if hasGap(filtered.Gaps, "record_without_allowed_fields_omitted") || gapDetail(filtered.Gaps, "cursor_incomplete_tail_omitted") != "1 of 7 messages, from the first one without a usable row on, are left for a later pass" {
		t.Errorf("gaps = %#v", filtered.Gaps)
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
	t.Parallel()
	filtered, _ := filterComposerFixture(t)
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

// oneMessageChat is a chat whose single header is b1 of the given type, with
// row as its message.
func oneMessageChat(headerType int, row string) CursorComposer {
	composer := fmt.Sprintf(`{"_v":18,"composerId":"c","createdAt":1790000000000,"fullConversationHeadersOnly":[{"bubbleId":"b1","type":%d}]}`, headerType)
	return CursorComposer{Composer: json.RawMessage(composer), Bubbles: []CursorBubble{{ID: "b1", Value: json.RawMessage(row)}}}
}

// Fail closed: only the probed versions (chat 18, message 3) are read. Any
// other _v on the chat or on any message, older or newer, refuses the whole
// chat as an unsafe format.
func TestCursorComposerRefusesUnknownVersions(t *testing.T) {
	t.Parallel()
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
		`{"_v":17,"composerId":"c"}`,       // older
		`{"_v":2,"composerId":"c","conversation":[{"_v":1,"bubbleId":"i1","type":1,"text":"x"}]}`, // an inline-conversation chat
	} {
		_, err := (CursorAdapter{}).FilterComposer(CursorComposer{Composer: json.RawMessage(composer)})
		if !errors.Is(err, ErrUnsafeSourceFormat) {
			t.Errorf("%s: err = %v", composer, err)
		}
	}
	for _, row := range []string{
		`{"bubbleId":"b1","type":1,"text":"x"}`,        // no _v
		`{"_v":2,"bubbleId":"b1","type":1,"text":"x"}`, // older
		`{"_v":4,"bubbleId":"b1","type":1,"text":"x"}`, // newer
	} {
		if _, err := (CursorAdapter{}).FilterComposer(oneMessageChat(1, row)); !errors.Is(err, ErrUnsafeSourceFormat) {
			t.Errorf("%s: err = %v", row, err)
		}
	}
}

// A chat with headers and an inline conversation too has the conversation
// reported rather than silently dropped.
func TestCursorComposerReportsAnInlineConversationBesideHeaders(t *testing.T) {
	t.Parallel()
	c := oneMessageChat(1, `{"_v":3,"bubbleId":"b1","type":1,"text":"hello"}`)
	c.Composer = json.RawMessage(`{"_v":18,"composerId":"c","fullConversationHeadersOnly":[{"bubbleId":"b1","type":1}],"conversation":[{"bubbleId":"old","type":1,"text":"INLINE-SENTINEL"}]}`)
	filtered, err := (CursorAdapter{}).FilterComposer(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Records) != 2 || bytes.Contains(bytes.Join(filtered.Records, nil), []byte("SENTINEL")) {
		t.Fatalf("records = %s", bytes.Join(filtered.Records, []byte("\n")))
	}
	if !hasGap(filtered.Gaps, "cursor_inline_conversation_omitted") {
		t.Fatalf("gaps = %#v", filtered.Gaps)
	}
	// An empty conversation array, as current chats carry, is not a gap.
	c.Composer = json.RawMessage(`{"_v":18,"composerId":"c","fullConversationHeadersOnly":[{"bubbleId":"b1","type":1}],"conversation":[]}`)
	if filtered, _ = (CursorAdapter{}).FilterComposer(c); hasGap(filtered.Gaps, "cursor_inline_conversation_omitted") {
		t.Fatalf("gaps = %#v", filtered.Gaps)
	}
}

// A chat without messages yields no records at all, not a lone session
// record, so the import plan classifies it as empty.
func TestCursorComposerEmptyChatHasNoRecords(t *testing.T) {
	t.Parallel()
	for _, composer := range []string{
		`{"_v":18,"composerId":"c","createdAt":1790000000000}`,
		`{"_v":18,"composerId":"c","createdAt":1790000000000,"fullConversationHeadersOnly":[]}`,
	} {
		filtered, err := (CursorAdapter{}).FilterComposer(CursorComposer{Composer: json.RawMessage(composer)})
		if err != nil {
			t.Fatal(err)
		}
		if len(filtered.Records) != 0 || filtered.Boundary.RetainedRecords != 0 {
			t.Fatalf("%s: records = %s", composer, bytes.Join(filtered.Records, []byte("\n")))
		}
	}
}

// liveMessage is one message of a synthetic chat in use. toolStatus, when
// set, gives it a tool call with that status. No message has completedAtMs:
// most finished assistant messages in a real database have none.
type liveMessage struct {
	id         string
	kind       int
	text       string
	toolStatus string
}

// liveChat builds a chat snapshot with the given composer status and
// generatingBubbleIds.
func liveChat(status string, generating []string, messages ...liveMessage) CursorComposer {
	var headers []string
	c := CursorComposer{}
	for i, m := range messages {
		headers = append(headers, fmt.Sprintf(`{"bubbleId":%q,"type":%d}`, m.id, m.kind))
		row := map[string]any{"_v": 3, "bubbleId": m.id, "type": m.kind, "text": m.text, "createdAt": 1790000001000 + 1000*i}
		if m.toolStatus != "" {
			tool := map[string]any{"toolCallId": "t-" + m.id, "name": "run_terminal_command_v2", "status": m.toolStatus, "params": `{"command":"go test ./..."}`}
			if m.toolStatus == "completed" {
				tool["result"] = "ok"
			}
			row["toolFormerData"] = tool
		}
		encoded, _ := json.Marshal(row)
		c.Bubbles = append(c.Bubbles, CursorBubble{ID: m.id, Value: encoded})
	}
	ids, _ := json.Marshal(generating)
	if generating == nil {
		ids = []byte("[]")
	}
	c.Composer = json.RawMessage(fmt.Sprintf(`{"_v":18,"composerId":"c","createdAt":1790000000000,"status":%q,"generatingBubbleIds":%s,"fullConversationHeadersOnly":[%s]}`, status, ids, strings.Join(headers, ",")))
	return c
}

// A finished chat is emitted whole even though none of its assistant
// messages has completedAtMs, and completedAtMs is not what decides it.
func TestCursorComposerFinishedChatWithoutCompletionTimes(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"completed", "none", ""} {
		filtered, err := (CursorAdapter{}).FilterComposer(liveChat(status, nil,
			liveMessage{"u1", 1, "Run the tests.", ""},
			liveMessage{"a1", 2, "", ""}, // a thinking-only message: no text
			liveMessage{"a2", 2, "Running them.", "completed"},
			liveMessage{"a3", 2, "One failed.", "error"},
			liveMessage{"a4", 2, "Done.", ""},
		))
		if err != nil {
			t.Fatal(err)
		}
		if len(filtered.Records) != 6 || hasGap(filtered.Gaps, "cursor_incomplete_tail_omitted") {
			t.Fatalf("status %q: %d records, gaps %#v", status, len(filtered.Records), filtered.Gaps)
		}
	}
}

// A message is in flight when the chat lists it in generatingBubbleIds, when
// its tool call has not settled (any status but completed, error, or
// cancelled, or none), or, as a backstop, when it is the last message of a
// chat whose status is not a settled one. Output stops there.
func TestCursorComposerInFlightMessages(t *testing.T) {
	t.Parallel()
	user := liveMessage{"u1", 1, "Run the tests.", ""}
	cases := []struct {
		name    string
		chat    CursorComposer
		records int
	}{
		{"generating ID", liveChat("completed", []string{"a1"}, user, liveMessage{"a1", 2, "Run", ""}, liveMessage{"a2", 2, "later", ""}), 2},
		{"generating ID without a row", liveChat("none", []string{"a1"}, user, liveMessage{"a1", 2, "", ""}), 2},
		{"tool loading", liveChat("none", nil, user, liveMessage{"a1", 2, "Running", "loading"}), 2},
		{"tool pending", liveChat("none", nil, user, liveMessage{"a1", 2, "Running", "pending"}), 2},
		{"tool status unknown", liveChat("none", nil, user, liveMessage{"a1", 2, "Running", "streaming"}), 2},
		{"chat generating", liveChat("generating", nil, user, liveMessage{"a1", 2, "Done.", ""}, liveMessage{"a2", 2, "Runn", ""}), 3},
		{"chat status unknown", liveChat("thinking", nil, user), 0},
		{"chat aborted", liveChat("aborted", nil, user, liveMessage{"a1", 2, "Cut off", ""}), 3},
		{"tool cancelled", liveChat("none", nil, user, liveMessage{"a1", 2, "Stopped", "cancelled"}), 3},
	}
	for _, tc := range cases {
		if tc.name == "generating ID without a row" {
			tc.chat.Bubbles[1].Value = nil
		}
		filtered, err := (CursorAdapter{}).FilterComposer(tc.chat)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		inFlight := hasGap(filtered.Gaps, "cursor_incomplete_tail_omitted")
		if len(filtered.Records) != tc.records || inFlight != (tc.records < len(tc.chat.Bubbles)+1) {
			t.Errorf("%s: %d records, gaps %#v", tc.name, len(filtered.Records), filtered.Gaps)
		}
		if hasGap(filtered.Gaps, "cursor_bubble_missing") {
			t.Errorf("%s: an in-flight message was counted as missing", tc.name)
		}
	}
	filtered, _ := (CursorAdapter{}).FilterComposer(cases[0].chat)
	if got := gapDetail(filtered.Gaps, "cursor_incomplete_tail_omitted"); got != "2 of 3 messages, from the first one still in flight on, are left for a later pass" {
		t.Fatalf("gap = %q", got)
	}
}

// Successive snapshots of chats in use filter to record sequences each of
// which is a prefix of the next, so the collector's append-only check holds.
func TestCursorComposerSnapshotsAreAppendOnly(t *testing.T) {
	t.Parallel()
	user1 := liveMessage{"u1", 1, "Run the tests.", ""}
	user2 := liveMessage{"u2", 1, "Thanks.", ""}
	sequences := map[string][]CursorComposer{
		// A reply streams while listed in generatingBubbleIds, then is
		// removed from it.
		"streaming reply": {
			liveChat("completed", nil, user1),
			liveChat("generating", []string{"a1"}, user1, liveMessage{"a1", 2, "", ""}),
			liveChat("generating", []string{"a1"}, user1, liveMessage{"a1", 2, "Running", ""}),
			liveChat("generating", []string{"a1"}, user1, liveMessage{"a1", 2, "Running the tests now.", ""}),
			liveChat("completed", nil, user1, liveMessage{"a1", 2, "Running the tests now.", ""}),
			liveChat("completed", nil, user1, liveMessage{"a1", 2, "Running the tests now.", ""}, user2),
		},
		// A tool call is loading, then completes.
		"tool call": {
			liveChat("none", nil, user1),
			liveChat("none", nil, user1, liveMessage{"a1", 2, "Running", "loading"}),
			liveChat("none", nil, user1, liveMessage{"a1", 2, "Running the tests", "loading"}),
			liveChat("none", nil, user1, liveMessage{"a1", 2, "Running the tests", "completed"}),
			liveChat("none", nil, user1, liveMessage{"a1", 2, "Running the tests", "completed"}, user2),
		},
		// Only the chat's status says it is generating.
		"status backstop": {
			liveChat("generating", nil, user1, liveMessage{"a1", 2, "Run", ""}),
			liveChat("generating", nil, user1, liveMessage{"a1", 2, "Running.", ""}),
			liveChat("completed", nil, user1, liveMessage{"a1", 2, "Running.", ""}),
		},
	}
	for name, snapshots := range sequences {
		var previous [][]byte
		grew := false
		for i, snapshot := range snapshots {
			filtered, err := (CursorAdapter{}).FilterComposer(snapshot)
			if err != nil {
				t.Fatalf("%s %d: %v", name, i, err)
			}
			if len(filtered.Records) < len(previous) {
				t.Fatalf("%s %d lost records", name, i)
			}
			for j := range previous {
				if !bytes.Equal(previous[j], filtered.Records[j]) {
					t.Fatalf("%s %d rewrote record %d:\n%s\n%s", name, i, j, previous[j], filtered.Records[j])
				}
			}
			grew = grew || len(filtered.Records) > len(previous) && len(previous) > 0
			previous = filtered.Records
		}
		if last := len(snapshots[len(snapshots)-1].Bubbles) + 1; len(previous) != last || !grew {
			t.Fatalf("%s: ended with %d records, want %d", name, len(previous), last)
		}
	}
}

// A row whose bubbleId is some other message's is treated as missing.
func TestCursorComposerRowForAnotherMessageIsMissing(t *testing.T) {
	t.Parallel()
	filtered, err := (CursorAdapter{}).FilterComposer(oneMessageChat(1, `{"_v":3,"bubbleId":"other","type":1,"text":"MISMATCH-SENTINEL"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Records) != 0 || gapDetail(filtered.Gaps, "cursor_bubble_missing") != "1 of 1 messages have no message row" || !hasGap(filtered.Gaps, "cursor_bubble_id_mismatch") {
		t.Fatalf("records = %s gaps = %#v", bytes.Join(filtered.Records, nil), filtered.Gaps)
	}
}

// Arguments come from rawArgs when it is an object, else params; when neither
// is, they are dropped and named. A string argument that is itself JSON, at
// any depth, is dropped and named; an empty argument object is dropped
// silently.
func TestCursorComposerToolArguments(t *testing.T) {
	t.Parallel()
	input := func(t *testing.T, tool string) (map[string]any, FilteredTranscript) {
		t.Helper()
		filtered, err := (CursorAdapter{}).FilterComposer(oneMessageChat(2, `{"_v":3,"bubbleId":"b1","type":2,"completedAtMs":1790000001000,"toolFormerData":`+tool+`}`))
		if err != nil {
			t.Fatal(err)
		}
		var record map[string]any
		if err := json.Unmarshal(filtered.Records[1], &record); err != nil {
			t.Fatal(err)
		}
		call := record["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
		args, _ := call["input"].(map[string]any)
		return args, filtered
	}

	args, filtered := input(t, `{"toolCallId":"t1","status":"completed","name":"run_terminal_command_v2","rawArgs":"{\"command\":\"go te","params":"{\"command\":\"go te"}`)
	if args != nil || gapDetail(filtered.Gaps, "cursor_tool_argument_omitted") != "tool arguments that do not decode to an object, from Cursor's: params, rawArgs" {
		t.Fatalf("undecodable arguments: input = %#v gaps = %#v", args, filtered.Gaps)
	}

	args, filtered = input(t, `{"toolCallId":"t1","status":"completed","name":"run_terminal_command_v2","rawArgs":"{\"command\":\"ls\",\"env\":[\"A=1\",\"[\\\"NESTED-SENTINEL\\\"]\"],\"options\":{\"deep\":{\"blob\":\"{\\\"x\\\":\\\"NESTED-SENTINEL\\\"}\"}}}"}`)
	if args["command"] != "ls" || len(args["env"].([]any)) != 1 || bytes.Contains(bytes.Join(filtered.Records, nil), []byte("SENTINEL")) {
		t.Fatalf("nested JSON: input = %#v", args)
	}
	if _, kept := args["options"]; kept {
		t.Fatalf("an object emptied by the nested-JSON rule was kept: %#v", args)
	}
	if got := gapDetail(filtered.Gaps, "cursor_tool_argument_omitted"); got != "omitted tool arguments: blob, env" {
		t.Fatalf("nested JSON gap = %q", got)
	}

	args, filtered = input(t, `{"toolCallId":"t1","status":"completed","name":"glob_file_search","rawArgs":"{}"}`)
	if args != nil || hasGap(filtered.Gaps, "record_without_allowed_fields_omitted") || hasGap(filtered.Gaps, "cursor_tool_argument_omitted") {
		t.Fatalf("empty arguments: input = %#v gaps = %#v", args, filtered.Gaps)
	}
}

// Malformed input and a message list that does not match the headers are
// refused with a FilterError, never filtered partially.
func TestCursorComposerRefusesMalformedInput(t *testing.T) {
	t.Parallel()
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

// A missing row (nil or JSON null), a row for another message, and a row
// whose type disagrees with its header each stop output at that message, with
// the specific gap and the held-back tail counted; nothing after it is
// emitted, and the chat ends at the last message that was.
func TestCursorComposerUnusableRowStopsOutput(t *testing.T) {
	t.Parallel()
	user := liveMessage{"u1", 1, "Run the tests.", ""}
	cases := map[string]struct {
		row  json.RawMessage
		code string
	}{
		"nil row":       {nil, "cursor_bubble_missing"},
		"null row":      {json.RawMessage(`null`), "cursor_bubble_missing"},
		"another's row": {json.RawMessage(`{"_v":3,"bubbleId":"other","type":2,"text":"MISMATCH-SENTINEL"}`), "cursor_bubble_id_mismatch"},
		"type mismatch": {json.RawMessage(`{"_v":3,"bubbleId":"a1","type":1,"text":"MISMATCH-SENTINEL"}`), "cursor_message_type_unknown"},
	}
	for name, tc := range cases {
		c := liveChat("completed", nil, user, liveMessage{"a1", 2, "", ""}, liveMessage{"u2", 1, "LATER-SENTINEL", ""})
		c.Bubbles[1].Value = tc.row
		filtered, err := (CursorAdapter{}).FilterComposer(c)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(filtered.Records) != 2 || bytes.Contains(bytes.Join(filtered.Records, nil), []byte("SENTINEL")) {
			t.Fatalf("%s: records = %s", name, bytes.Join(filtered.Records, []byte("\n")))
		}
		if !hasGap(filtered.Gaps, tc.code) || gapDetail(filtered.Gaps, "cursor_incomplete_tail_omitted") != "2 of 3 messages, from the first one without a usable row on, are left for a later pass" {
			t.Fatalf("%s: gaps = %#v", name, filtered.Gaps)
		}
		if end := time.UnixMilli(1790000001000).UTC(); !filtered.NativeEndAt.Equal(end) {
			t.Fatalf("%s: end = %v", name, filtered.NativeEndAt)
		}
	}
}

// A row that is missing mid-chat and appears later leaves the output
// append-only: u1, (a1 missing), u2 and then u1, a1, u2.
func TestCursorComposerMissingRowThenPresentIsAppendOnly(t *testing.T) {
	t.Parallel()
	messages := []liveMessage{{"u1", 1, "Run the tests.", ""}, {"a1", 2, "Done.", ""}, {"u2", 1, "Thanks.", ""}}
	before := liveChat("completed", nil, messages...)
	before.Bubbles[1].Value = nil
	after := liveChat("completed", nil, messages...)
	first, err := (CursorAdapter{}).FilterComposer(before)
	if err != nil {
		t.Fatal(err)
	}
	second, err := (CursorAdapter{}).FilterComposer(after)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Records) != 2 || len(second.Records) != 4 {
		t.Fatalf("records: %d then %d", len(first.Records), len(second.Records))
	}
	for i := range first.Records {
		if !bytes.Equal(first.Records[i], second.Records[i]) {
			t.Fatalf("record %d rewritten:\n%s\n%s", i, first.Records[i], second.Records[i])
		}
	}
}

// toolMessage filters a chat whose one assistant message carries tool and
// returns that message's content blocks.
func toolMessage(t *testing.T, tool string) ([]any, FilteredTranscript) {
	t.Helper()
	filtered, err := (CursorAdapter{}).FilterComposer(oneMessageChat(2, `{"_v":3,"bubbleId":"b1","type":2,"toolFormerData":`+tool+`}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Records) != 2 {
		t.Fatalf("records = %s", bytes.Join(filtered.Records, []byte("\n")))
	}
	var record map[string]any
	if err := json.Unmarshal(filtered.Records[1], &record); err != nil {
		t.Fatal(err)
	}
	return record["message"].(map[string]any)["content"].([]any), filtered
}

// An empty rawArgs object does not hide params (S1), and nothing is lost
// when both are empty.
func TestCursorComposerEmptyRawArgsFallsBackToParams(t *testing.T) {
	t.Parallel()
	content, filtered := toolMessage(t, `{"toolCallId":"t1","status":"completed","name":"web_search","rawArgs":"{}","params":"{\"searchTerm\":\"widget parser\"}","result":"ok"}`)
	input, _ := content[0].(map[string]any)["input"].(map[string]any)
	if input["searchTerm"] != "widget parser" || hasGap(filtered.Gaps, "cursor_tool_argument_omitted") {
		t.Fatalf("input = %#v gaps = %#v", input, filtered.Gaps)
	}
	// rawArgs whose only argument was nested JSON also falls back.
	content, filtered = toolMessage(t, `{"toolCallId":"t1","status":"completed","name":"web_fetch","rawArgs":"{\"policy\":\"{\\\"a\\\":1}\"}","params":"{\"url\":\"https://example.com\"}","result":"ok"}`)
	if input, _ = content[0].(map[string]any)["input"].(map[string]any); input["url"] != "https://example.com" {
		t.Fatalf("input = %#v", input)
	}
	if got := gapDetail(filtered.Gaps, "cursor_tool_argument_omitted"); got != "omitted tool arguments: policy" {
		t.Fatalf("gap = %q", got)
	}
}

// An error that is not a string (S2) is kept as text like a string one: the
// error is the content, is_error is set, and the result beside it is named.
func TestCursorComposerStructuredToolError(t *testing.T) {
	t.Parallel()
	content, filtered := toolMessage(t, `{"toolCallId":"t1","status":"error","name":"read_file_v2","params":"{\"path\":\"a.go\"}","result":"RESULT-BESIDE-ERROR-SENTINEL","error":{"message":"file not found","code":2}}`)
	result := content[1].(map[string]any)
	if result["content"] != `{"code":2,"message":"file not found"}` || result["is_error"] != true || result["tool_use_id"] != "t1" {
		t.Fatalf("result = %#v", result)
	}
	if bytes.Contains(bytes.Join(filtered.Records, nil), []byte("SENTINEL")) || !strings.Contains(gapDetail(filtered.Gaps, "unknown_field_omitted"), "tool.result") {
		t.Fatalf("records = %s gaps = %#v", bytes.Join(filtered.Records, nil), filtered.Gaps)
	}
	// An empty error is no error.
	content, _ = toolMessage(t, `{"toolCallId":"t1","status":"completed","name":"read_file_v2","result":"ok","error":""}`)
	if result := content[len(content)-1].(map[string]any); result["content"] != "ok" || result["is_error"] != nil {
		t.Fatalf("result = %#v", result)
	}
}

// Nested JSON deep inside arrays is named by the nearest argument key, and
// the arrays it leaves empty are removed rather than kept as [[]].
func TestCursorComposerNestedJSONInArrays(t *testing.T) {
	t.Parallel()
	content, filtered := toolMessage(t, `{"toolCallId":"t1","status":"completed","name":"x","params":{"keep":"yes","matrix":[["{\"a\":\"NESTED-SENTINEL\"}"]],"rows":[["ok","[1,2]"]],"empty":[]}}`)
	input := content[0].(map[string]any)["input"].(map[string]any)
	if _, kept := input["matrix"]; kept {
		t.Fatalf("an emptied array was kept: %#v", input)
	}
	if rows, _ := json.Marshal(input["rows"]); string(rows) != `[["ok"]]` {
		t.Fatalf("rows = %s", rows)
	}
	if empty, ok := input["empty"].([]any); !ok || len(empty) != 0 {
		t.Fatalf("an originally empty array was not kept: %#v", input)
	}
	if got := gapDetail(filtered.Gaps, "cursor_tool_argument_omitted"); got != "omitted tool arguments: matrix, rows" || bytes.Contains(bytes.Join(filtered.Records, nil), []byte("SENTINEL")) {
		t.Fatalf("gap = %q", got)
	}
}

// A typed-input tool's text and a credential-named argument are dropped by
// the shared tool filter, exactly as in a JSONL transcript.
func TestCursorComposerToolArgumentsUseTheSharedDenyList(t *testing.T) {
	t.Parallel()
	c := CursorComposer{
		Composer: json.RawMessage(`{"_v":18,"composerId":"c","fullConversationHeadersOnly":[{"bubbleId":"b1","type":2}]}`),
		Bubbles:  []CursorBubble{{ID: "b1", Value: json.RawMessage(`{"_v":3,"bubbleId":"b1","type":2,"completedAtMs":1790000001000,"toolFormerData":{"toolCallId":"t1","name":"mcp__browser__type","status":"error","params":{"selector":"#login","text":"TYPED-SENTINEL","apiToken":"ARG-SENTINEL"},"result":"failed"}}`)}},
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
	t.Parallel()
	filtered, _ := filterComposerFixture(t)
	reg := registration()
	reg.Harness = Harness{Name: "cursor"}
	bundle, err := NewSourceBundle(reg, CursorAdapter{}, filtered, time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Capture.SourceFormat != "cursor-composer" || bundle.Capture.FilterVersion != FilterVersion {
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
		case TurnKindToolResult, TurnKindHarnessMeta, TurnKindCommandOutput, TurnKindShellCommand,
			TurnKindLocalCommand, TurnKindCompactSummary, TurnKindHarnessNotification:
		}
	}
	if prompts != 2 || replies != 3 {
		t.Fatalf("prompts = %d replies = %d: %#v", prompts, replies, view.Turns)
	}
	if len(view.ToolCalls) != 2 || view.ToolCalls[0].Name != "read_file" || view.ToolCalls[0].CallID != "call-1" || view.ToolCalls[1].CallID != "call-2" {
		t.Fatalf("tool calls = %#v", view.ToolCalls)
	}
	if len(view.ToolResults) != 3 || !view.ToolResults[2].IsError {
		t.Fatalf("tool results = %#v", view.ToolResults)
	}
	if view.Tokens.Input == nil || *view.Tokens.Input != 2600 || view.Tokens.Output == nil || *view.Tokens.Output != 65 {
		t.Fatalf("tokens = %#v", view.Tokens)
	}
}
