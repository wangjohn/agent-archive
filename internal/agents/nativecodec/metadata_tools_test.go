package nativecodec

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func namedCalls(counts map[string]int) []NormalizedToolCall {
	var calls []NormalizedToolCall
	for name, count := range counts {
		for range count {
			calls = append(calls, NormalizedToolCall{Name: name})
		}
	}
	return calls
}

// tools_used is ordered by count descending, then name ascending, and holds
// at most MaxToolsUsed tools.
func TestDeriveToolsUsedSortsBreaksTiesAndCaps(t *testing.T) {
	t.Parallel()
	counts := map[string]int{"Read": 5, "Bash": 5, "Edit": 3, "mcp__github__get_issue": 3}
	for i := range 8 {
		counts[fmt.Sprintf("tool_%02d", i)] = 1
	}
	got := deriveToolsUsed(namedCalls(counts), "")
	want := []ToolUsage{
		{"Bash", 5}, {"Read", 5}, {"Edit", 3}, {"mcp__github__get_issue", 3},
		{"tool_00", 1}, {"tool_01", 1}, {"tool_02", 1}, {"tool_03", 1}, {"tool_04", 1}, {"tool_05", 1},
	}
	if len(want) != MaxToolsUsed {
		t.Fatalf("test expects the cap to be %d", len(want))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tools used = %v\nwant %v", got, want)
	}
	if got := deriveToolsUsed(nil, ""); got != nil {
		t.Fatalf("no calls = %v, want nil", got)
	}
}

// A Codex local_shell_call, filtered, has neither a name nor arguments (the
// filter drops its action); it is counted under its record type. Its
// completion echo is deduplicated, and named completions count by name.
func TestDeriveToolsUsedNamelessCalls(t *testing.T) {
	t.Parallel()
	filtered, err := CodexAdapter{}.FilterJSONL(strings.NewReader(string(fixture(t, "codex-tool-events.jsonl"))))
	if err != nil {
		t.Fatal(err)
	}
	m := parserTestMetadata(t, parserTestBundle(t, "codex", CodexAdapter{}, filtered))
	want := []ToolUsage{{"local_shell_call", 1}, {"search_docs", 1}, {"widget-extension", 1}}
	if !reflect.DeepEqual(m.ToolsUsed, want) {
		t.Fatalf("tools used = %v, want %v", m.ToolsUsed, want)
	}
	// Handoff lists this session's calls under the same names.
	handoff, err := BuildHandoff(parserTestBundle(t, "codex", CodexAdapter{}, filtered), &m, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := handoffToolNames(handoff); !reflect.DeepEqual(got, []string{"local_shell_call", "search_docs", "widget-extension"}) {
		t.Fatalf("handoff tools = %v", got)
	}
}

func handoffToolNames(h Handoff) []string {
	var names []string
	for _, exchange := range h.Exchanges {
		for _, step := range exchange.Steps {
			if step.Tool != nil {
				names = append(names, step.Tool.Name)
			}
		}
	}
	return names
}

// tools_used counts exactly the calls handoff lists: a nameless completion
// that no invocation reported counts when it shows what ran (a
// CommandExecution's command), not when it shows nothing (an Extension
// item); a nameless invocation counts even with no arguments; a blank name
// is not published.
func TestToolsUsedAndHandoffListTheSameCalls(t *testing.T) {
	t.Parallel()
	lines := []string{
		`{"type":"response_item","timestamp":"2026-09-20T12:00:00Z","payload":{"type":"local_shell_call","call_id":"call_1","status":"completed","action":{"type":"exec","command":["ls"]}}}`,
		`{"type":"event_msg","timestamp":"2026-09-20T12:00:01Z","payload":{"type":"item_completed","item":{"id":"call_1","type":"CommandExecution","command":"ls","status":"completed"}}}`,
		`{"type":"event_msg","timestamp":"2026-09-20T12:00:02Z","payload":{"type":"item_completed","item":{"id":"item_2","type":"CommandExecution","command":"go test ./...","status":"completed"}}}`,
		`{"type":"event_msg","timestamp":"2026-09-20T12:00:03Z","payload":{"type":"item_completed","item":{"id":"item_3","type":"Extension","status":"completed"}}}`,
	}
	filtered, err := CodexAdapter{}.FilterJSONL(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	bundle := parserTestBundle(t, "codex", CodexAdapter{}, filtered)
	m := parserTestMetadata(t, bundle)
	want := []ToolUsage{{"CommandExecution", 1}, {"local_shell_call", 1}}
	if !reflect.DeepEqual(m.ToolsUsed, want) {
		t.Fatalf("tools used = %v, want %v (tool_calls %v)", m.ToolsUsed, want, *m.Counts.ToolCalls)
	}
	handoff, err := BuildHandoff(bundle, &m, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := handoffToolNames(handoff); !reflect.DeepEqual(got, []string{"local_shell_call", "CommandExecution"}) {
		t.Fatalf("handoff tools = %v", got)
	}
	if got := deriveToolsUsed([]NormalizedToolCall{{Name: "   ", Invocation: true, ObservedName: "   "}}, ""); got != nil {
		t.Fatalf("blank name = %v", got)
	}
}

// A subagent's records inlined in a parent transcript belong to the child's
// session: they neither count nor set the parent's ended_at.
func TestEndedAtIgnoresSidechainRecords(t *testing.T) {
	t.Parallel()
	bundle := claudeLines(t,
		`{"type":"user","timestamp":"2026-09-24T10:00:00Z","message":{"role":"user","content":"delegate"}}`,
		`{"type":"assistant","timestamp":"2026-09-24T10:01:00Z","message":{"id":"m1","role":"assistant","content":"started it"}}`,
		`{"type":"assistant","isSidechain":true,"timestamp":"2026-09-24T12:00:00Z","message":{"id":"m2","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Edit","input":{"file_path":"a.go"}}]}}`,
	)
	m, err := BuildMetadata(bundle, "machine", time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC), time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC), SourceReference{Key: "k", SHA256: strings.Repeat("a", 64)}, ParserInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 9, 24, 10, 1, 0, 0, time.UTC); m.EndedAt == nil || !m.EndedAt.Equal(want) {
		t.Fatalf("ended at = %v, want %v", m.EndedAt, want)
	}
	if m.ToolsUsed != nil || m.Counts.FilesTouched == nil || *m.Counts.FilesTouched != 0 {
		t.Fatalf("tools used = %v, files touched = %v", m.ToolsUsed, m.Counts.FilesTouched)
	}
}

// A tool name is bounded, one line, free of control characters, and
// credential-redacted before it is published.
func TestMetadataToolNameIsSafeToPublish(t *testing.T) {
	t.Parallel()
	if got := metadataToolName("mcp__github__create_pull_request"); got != "mcp__github__create_pull_request" {
		t.Errorf("MCP name = %q", got)
	}
	if got := metadataToolName(" run\x1b[31m\tthis\r\nnow "); got != "run[31m this now" {
		t.Errorf("control characters = %q", got)
	}
	long := metadataToolName(strings.Repeat("é", 500))
	if n := utf8.RuneCountInString(long); n != toolNameLimit || !strings.HasSuffix(long, "…") {
		t.Errorf("long name has %d runes: %q", n, long)
	}
	secret := "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	if got := metadataToolName("export GH=" + secret); strings.Contains(got, secret) {
		t.Errorf("credential kept: %q", got)
	}
}

// counts.files_touched counts distinct files the editing calls named: one
// file edited twice, or named absolutely and relative to the workspace, is
// one. A read is not a touch. Handoff lists the same files.
func TestFilesTouchedCountsDistinctFilesAndAgreesWithHandoff(t *testing.T) {
	t.Parallel()
	bundle := claudeLines(t,
		`{"type":"user","cwd":"/work/widget","timestamp":"2026-09-24T10:00:00Z","message":{"role":"user","content":"fix it"}}`,
		`{"type":"assistant","cwd":"/work/widget","timestamp":"2026-09-24T10:00:01Z","message":{"id":"m1","role":"assistant","content":[`+
			`{"type":"tool_use","id":"t1","name":"Edit","input":{"file_path":"/work/widget/a.go","old_string":"x","new_string":"y"}},`+
			`{"type":"tool_use","id":"t2","name":"Write","input":{"file_path":"a.go","content":"z"}},`+
			`{"type":"tool_use","id":"t3","name":"Edit","input":{"file_path":"./a.go","old_string":"y","new_string":"w"}},`+
			`{"type":"tool_use","id":"t4","name":"MultiEdit","input":{"file_path":"/work/widget/sub/b.go","edits":[]}},`+
			`{"type":"tool_use","id":"t5","name":"Read","input":{"file_path":"/work/widget/c.go"}},`+
			`{"type":"tool_use","id":"t6","name":"Write","input":{"file_path":"/elsewhere/d.go","content":"q"}}]}}`,
	)
	metadata := parserTestMetadata(t, bundle)
	if metadata.Counts.FilesTouched == nil || *metadata.Counts.FilesTouched != 3 {
		t.Fatalf("files touched = %v, want 3", metadata.Counts.FilesTouched)
	}
	handoff, err := BuildHandoff(bundle, &metadata, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.go", "sub/b.go", "/elsewhere/d.go"}; !reflect.DeepEqual(handoff.FilesTouched, want) {
		t.Fatalf("handoff files = %v, want %v", handoff.FilesTouched, want)
	}
	want := []ToolUsage{{"Edit", 2}, {"Write", 2}, {"MultiEdit", 1}, {"Read", 1}}
	if !reflect.DeepEqual(metadata.ToolsUsed, want) {
		t.Fatalf("tools used = %v, want %v", metadata.ToolsUsed, want)
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"a.go", "b.go", "d.go", "/work/widget"} {
		if strings.Contains(string(data), path) {
			t.Errorf("metadata contains the path %q: %s", path, data)
		}
	}
}

// A known session with no editing call touched zero files; a session whose
// structure is unknown (a Cursor text transcript) leaves the count and
// tools_used absent.
func TestFilesTouchedZeroVersusUnknown(t *testing.T) {
	t.Parallel()
	known := parserTestMetadata(t, claudeLines(t,
		`{"type":"user","timestamp":"2026-09-24T10:00:00Z","message":{"role":"user","content":"hello"}}`,
	))
	if known.Counts.FilesTouched == nil || *known.Counts.FilesTouched != 0 || known.ToolsUsed != nil {
		t.Fatalf("known session: files touched = %v, tools used = %v", known.Counts.FilesTouched, known.ToolsUsed)
	}
	filtered, err := CursorAdapter{}.FilterText(strings.NewReader("user:\nedit a.go\n\nassistant:\ndone\n"), time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	text := parserTestMetadata(t, parserTestBundle(t, "cursor", CursorAdapter{}, filtered))
	if text.Counts.FilesTouched != nil || text.ToolsUsed != nil || text.EndedAt != nil {
		t.Fatalf("text transcript: files touched = %v, tools used = %v, ended at = %v", text.Counts.FilesTouched, text.ToolsUsed, text.EndedAt)
	}
}

// Codex's apply_patch names its files in the patch headers.
func TestFilesTouchedReadsCodexPatches(t *testing.T) {
	t.Parallel()
	filtered, err := CodexAdapter{}.FilterJSONL(strings.NewReader(string(fixture(t, "handoff/codex.jsonl"))))
	if err != nil {
		t.Fatal(err)
	}
	bundle := parserTestBundle(t, "codex", CodexAdapter{}, filtered)
	metadata := parserTestMetadata(t, bundle)
	handoff, err := BuildHandoff(bundle, &metadata, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Counts.FilesTouched == nil || *metadata.Counts.FilesTouched != len(handoff.FilesTouched) || len(handoff.FilesTouched) < 2 {
		t.Fatalf("files touched = %v, handoff lists %v", metadata.Counts.FilesTouched, handoff.FilesTouched)
	}
	found := false
	for _, tool := range metadata.ToolsUsed {
		found = found || tool.Name == "apply_patch"
	}
	if !found {
		t.Fatalf("tools used = %v, want apply_patch", metadata.ToolsUsed)
	}
}

// ended_at is the latest record timestamp, whatever order the records are
// in, and never earlier than started_at.
func TestEndedAtIsTheLatestRecordTimestamp(t *testing.T) {
	t.Parallel()
	bundle := claudeLines(t,
		`{"type":"user","timestamp":"2026-09-24T10:00:00Z","message":{"role":"user","content":"go"}}`,
		`{"type":"assistant","timestamp":"2026-09-24T10:05:00.5Z","message":{"id":"m1","role":"assistant","content":"later"}}`,
		`{"type":"assistant","timestamp":"2026-09-24T10:03:00Z","message":{"id":"m2","role":"assistant","content":"out of order"}}`,
		`{"type":"system","subtype":"compact_boundary","uuid":"u1","timestamp":"2026-09-24T10:04:00Z"}`,
	)
	build := func(startedAt time.Time) Metadata {
		m, err := BuildMetadata(bundle, "machine", startedAt, startedAt.Add(time.Hour), SourceReference{Key: "k", SHA256: strings.Repeat("a", 64)}, ParserInfo{})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := build(time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC))
	if want := time.Date(2026, 9, 24, 10, 5, 0, 5e8, time.UTC); m.EndedAt == nil || !m.EndedAt.Equal(want) {
		t.Fatalf("ended at = %v, want %v", m.EndedAt, want)
	}
	// A start recorded by a clock ahead of the transcript's.
	skewed := time.Date(2026, 9, 24, 11, 0, 0, 0, time.UTC)
	if m := build(skewed); m.EndedAt == nil || !m.EndedAt.Equal(skewed) {
		t.Fatalf("skewed ended at = %v, want %v", m.EndedAt, skewed)
	}
}

func TestEndedAtIsAbsentWithoutTimestamps(t *testing.T) {
	t.Parallel()
	filtered, err := CursorAdapter{}.FilterJSONL(strings.NewReader(string(fixture(t, "cursor-turn.jsonl"))))
	if err != nil {
		t.Fatal(err)
	}
	m := parserTestMetadata(t, parserTestBundle(t, "cursor", CursorAdapter{}, filtered))
	if m.EndedAt != nil {
		t.Fatalf("ended at = %v, want absent", m.EndedAt)
	}
	if len(m.ToolsUsed) == 0 || m.Counts.FilesTouched == nil {
		t.Fatalf("a Cursor JSONL transcript still has tool calls: tools used = %v, files touched = %v", m.ToolsUsed, m.Counts.FilesTouched)
	}
	data, _ := json.Marshal(m)
	if strings.Contains(string(data), "ended_at") {
		t.Fatalf("ended_at serialized: %s", data)
	}
}

// A Cursor database chat carries message times, tool names, and edits.
func TestCursorComposerMetadataHasEndTimeAndTools(t *testing.T) {
	t.Parallel()
	filtered, _ := filterComposerFixture(t)
	m := parserTestMetadata(t, parserTestBundle(t, "cursor", CursorAdapter{}, filtered))
	if m.EndedAt == nil || !m.EndedAt.Equal(filtered.NativeEndAt) {
		t.Fatalf("ended at = %v, want %v", m.EndedAt, filtered.NativeEndAt)
	}
	// The chat reads a file and runs a command: it edits nothing.
	if want := []ToolUsage{{"read_file", 1}, {"run_terminal_command_v2", 1}}; !reflect.DeepEqual(m.ToolsUsed, want) {
		t.Fatalf("tools used = %v, want %v", m.ToolsUsed, want)
	}
	if m.Counts.FilesTouched == nil || *m.Counts.FilesTouched != 0 {
		t.Fatalf("files touched = %v, want 0", m.Counts.FilesTouched)
	}
}

// Cursor's database chats edit with edit_file_v2, whose params name the file
// as relativeWorkspacePath. Two edits of one file, spelled two ways, touch
// one file, and handoff lists it and summarizes each call.
func TestCursorComposerEditFileV2TouchesFiles(t *testing.T) {
	t.Parallel()
	filtered, err := (CursorAdapter{}).FilterComposer(loadComposerFixture(t, "edit-file-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	bundle := parserTestBundle(t, "cursor", CursorAdapter{}, filtered)
	m := parserTestMetadata(t, bundle)
	if m.Counts.FilesTouched == nil || *m.Counts.FilesTouched != 1 {
		t.Fatalf("files touched = %v, want 1", m.Counts.FilesTouched)
	}
	if want := []ToolUsage{{"edit_file_v2", 2}, {"glob_file_search", 1}, {"read_file_v2", 1}, {"ripgrep_raw_search", 1}}; !reflect.DeepEqual(m.ToolsUsed, want) {
		t.Fatalf("tools used = %v, want %v", m.ToolsUsed, want)
	}
	handoff, err := BuildHandoff(bundle, &m, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"internal/widget/widget.go"}; !reflect.DeepEqual(handoff.FilesTouched, want) {
		t.Fatalf("handoff files = %v, want %v", handoff.FilesTouched, want)
	}
	var summaries []string
	for _, exchange := range handoff.Exchanges {
		for _, step := range exchange.Steps {
			if step.Tool != nil {
				summaries = append(summaries, step.Tool.Name+": "+step.Tool.Summary)
			}
		}
	}
	want := []string{
		"ripgrep_raw_search: parts in internal/widget",
		"read_file_v2: internal/widget/widget.go",
		"edit_file_v2: internal/widget/widget.go",
		"edit_file_v2: internal/widget/widget.go",
		"glob_file_search: *_test.go in internal/widget",
	}
	if !reflect.DeepEqual(summaries, want) {
		t.Fatalf("summaries = %q, want %q", summaries, want)
	}
}

// A call naming the workspace root itself shows it as "."; a search of the
// root leaves out "in <root>"; a relative path above the root keeps its
// relative spelling rather than printing the directories above the
// workspace.
func TestToolSummaryPathsAtAndAboveTheRoot(t *testing.T) {
	t.Parallel()
	root := "/Users/someone/work/widget"
	for _, tc := range []struct {
		name  string
		input map[string]any
		want  string
	}{
		{"Read", map[string]any{"file_path": "."}, "."},
		{"read_file", map[string]any{"target_file": root}, "."},
		{"Write", map[string]any{"file_path": root + "/"}, "."},
		{"Edit", map[string]any{"file_path": "../other/a.go"}, "../other/a.go"},
		{"Grep", map[string]any{"pattern": "TODO", "path": "."}, "TODO"},
		{"Grep", map[string]any{"pattern": "TODO", "path": root}, "TODO"},
		{"Grep", map[string]any{"pattern": "TODO", "path": "../other"}, "TODO in ../other"},
	} {
		if got := toolSummary(tc.name, tc.input, nil, root); got != tc.want {
			t.Errorf("toolSummary(%s, %v) = %q, want %q", tc.name, tc.input, got, tc.want)
		}
	}
}

// The schema accepts what the code can write at its limits (MaxToolsUsed
// tools, a toolNameLimit-rune name) and rejects anything past them.
func TestMetadataSchemaBoundsToolsUsed(t *testing.T) {
	t.Parallel()
	schema := compileSchema(t, "metadata.schema.json")
	base := parserTestMetadata(t, claudeLines(t,
		`{"type":"user","timestamp":"2026-09-24T10:00:00Z","message":{"role":"user","content":"go"}}`,
	))
	long := strings.Repeat("é", 1000)
	calls := []NormalizedToolCall{{Name: long}, {Name: long}}
	for i := range MaxToolsUsed + 5 {
		calls = append(calls, NormalizedToolCall{Name: fmt.Sprintf("mcp__server__tool_%02d", i)})
	}
	base.ToolsUsed = deriveToolsUsed(calls, "")
	validate := func(m Metadata) error {
		t.Helper()
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var instance any
		if err := json.Unmarshal(data, &instance); err != nil {
			t.Fatal(err)
		}
		return schema.Validate(instance)
	}
	if len(base.ToolsUsed) != MaxToolsUsed || utf8.RuneCountInString(base.ToolsUsed[0].Name) != toolNameLimit || base.EndedAt == nil || base.Counts.FilesTouched == nil {
		t.Fatalf("metadata = %+v", base)
	}
	if err := validate(base); err != nil {
		t.Fatalf("metadata at the limits is invalid: %v", err)
	}
	for name, mutate := range map[string]func(*Metadata){
		"too many tools": func(m *Metadata) { m.ToolsUsed = append(m.ToolsUsed, ToolUsage{"extra", 1}) },
		"zero count":     func(m *Metadata) { m.ToolsUsed[0].Count = 0 },
		"empty name":     func(m *Metadata) { m.ToolsUsed[0].Name = "" },
		"long name":      func(m *Metadata) { m.ToolsUsed[0].Name = strings.Repeat("x", toolNameLimit+1) },
		"negative files": func(m *Metadata) { n := -1; m.Counts.FilesTouched = &n },
	} {
		m := base
		m.ToolsUsed = append([]ToolUsage(nil), base.ToolsUsed...)
		mutate(&m)
		if validate(m) == nil {
			t.Errorf("%s: schema accepted it", name)
		}
	}
}
