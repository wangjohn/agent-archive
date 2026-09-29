package archive

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
	got := deriveToolsUsed(namedCalls(counts))
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
	if got := deriveToolsUsed(nil); got != nil {
		t.Fatalf("no calls = %v, want nil", got)
	}
}

// A call with no name of its own is listed under its record type, as
// handoff lists it; one with neither a name nor arguments names no tool.
func TestDeriveToolsUsedNamelessCalls(t *testing.T) {
	t.Parallel()
	calls := []NormalizedToolCall{
		{raw: map[string]any{"type": "local_shell_call"}, Input: map[string]any{"command": []any{"ls"}}},
		{raw: map[string]any{"type": "extension"}},
		{Name: "   "},
	}
	want := []ToolUsage{{"local_shell_call", 1}}
	if got := deriveToolsUsed(calls); !reflect.DeepEqual(got, want) {
		t.Fatalf("tools used = %v, want %v", got, want)
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
	if len(m.ToolsUsed) == 0 || m.Counts.FilesTouched == nil {
		t.Fatalf("tools used = %v, files touched = %v", m.ToolsUsed, m.Counts.FilesTouched)
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
	base.ToolsUsed = deriveToolsUsed(calls)
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
