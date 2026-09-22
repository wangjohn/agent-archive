package archive

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func turnKinds(view NormalizedView) map[int]TurnKind {
	kinds := map[int]TurnKind{}
	for _, turn := range view.Turns {
		kinds[turn.RecordIndex] = turn.Kind
	}
	return kinds
}

// Codex starts its own shell for a session and reports it as a completed
// CommandExecution with source unified_exec_startup. That is not a tool call
// the model made.
func TestParserV07ExcludesCodexStartupShell(t *testing.T) {
	view, metadata := parsedFixture(t, "codex", "codex-startup-shell.jsonl")
	countIs(t, "tool calls", metadata.Counts.ToolCalls, 2)
	ids := map[string]bool{}
	for _, call := range view.ToolCalls {
		ids[call.CallID] = true
	}
	if ids["item_startup_1"] {
		t.Fatalf("startup shell counted as a tool call: %#v", view.ToolCalls)
	}
	if !ids["call_tests_1"] || !ids["item_model_cmd_1"] {
		t.Fatalf("model commands missing: %#v", view.ToolCalls)
	}
	countIs(t, "turns", metadata.Counts.Turns, 1)
}

// Claude Code streams one response as several records sharing message.id.
// It is one message, and one turn for its model.
func TestParserV07CountsStreamedResponsesOnce(t *testing.T) {
	_, metadata := parsedFixture(t, "claude", "claude-streamed-response.jsonl")
	countIs(t, "turns", metadata.Counts.Turns, 1)
	countIs(t, "messages", metadata.Counts.Messages, 3)
	countIs(t, "tool calls", metadata.Counts.ToolCalls, 1)
	countIs(t, "tool results", metadata.Counts.ToolResults, 1)
	countIs(t, "input tokens", metadata.Counts.InputTokens, 250)
	countIs(t, "output tokens", metadata.Counts.OutputTokens, 60)
	if len(metadata.Models) != 1 || metadata.Models[0].TurnCount == nil || *metadata.Models[0].TurnCount != 2 {
		t.Fatalf("model turn counts = %#v", metadata.Models)
	}
}

// Assistant records without a message id cannot be grouped and each counts.
func TestParserV07AssistantRecordsWithoutIDsCountIndividually(t *testing.T) {
	now := time.Date(2026, 9, 22, 15, 0, 0, 0, time.UTC)
	bundle := SourceBundle{
		SchemaVersion: 1, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p",
		Capture: SourceCapture{Harness: Harness{Name: "claude"}, AdapterName: "claude", AdapterVersion: adapterVersion, SourceFormat: "claude-jsonl", FilterVersion: FilterVersion, CapturedAt: now},
		NativeRecords: []map[string]any{
			{"type": "user", "message": map[string]any{"role": "user", "content": "go"}},
			{"type": "assistant", "message": map[string]any{"role": "assistant", "content": "one"}},
			{"type": "assistant", "message": map[string]any{"role": "assistant", "content": "two"}},
		},
	}
	metadata, err := BuildMetadata(bundle, "m", now, now, SourceReference{Key: "sessions/claude/a/source." + strings.Repeat("a", 64) + ".json.gz", SHA256: strings.Repeat("a", 64)}, ParserInfo{})
	if err != nil {
		t.Fatal(err)
	}
	countIs(t, "messages", metadata.Counts.Messages, 3)
}

// /model is a local command: its caveat is isMeta, its output is
// <local-command-stdout>, and no assistant answers it. None of it is a prompt.
func TestParserV07LocalCommandIsNotAPrompt(t *testing.T) {
	view, metadata := parsedFixture(t, "claude", "claude-local-command.jsonl")
	countIs(t, "turns", metadata.Counts.Turns, 1)
	countIs(t, "messages", metadata.Counts.Messages, 2)
	countIs(t, "user shell commands", metadata.Counts.UserShellCommands, 0)
	kinds := turnKinds(view)
	if kinds[0] != TurnKindLocalCommand || kinds[2] != TurnKindCommandOutput || kinds[3] != TurnKindHumanPrompt || kinds[4] != TurnKindAssistant {
		t.Fatalf("turn kinds = %#v", kinds)
	}
	if _, visible := kinds[1]; visible {
		t.Fatalf("the stripped isMeta caveat produced a turn: %#v", kinds)
	}
}

// A skill slash command expands into an isMeta record and is answered by the
// assistant: the command is the prompt, the expansion is not.
func TestParserV07AnsweredSlashCommandIsAPrompt(t *testing.T) {
	view, metadata := parsedFixture(t, "claude", "claude-skill-command.jsonl")
	countIs(t, "turns", metadata.Counts.Turns, 1)
	countIs(t, "messages", metadata.Counts.Messages, 2)
	kinds := turnKinds(view)
	if kinds[0] != TurnKindHumanPrompt || kinds[2] != TurnKindAssistant {
		t.Fatalf("turn kinds = %#v", kinds)
	}
	if _, visible := kinds[1]; visible {
		t.Fatalf("the stripped isMeta expansion produced a turn: %#v", kinds)
	}
}

// A `!` command is the person running a shell command directly: not a prompt,
// not a model tool call, and counted on its own.
func TestParserV07CountsUserShellCommands(t *testing.T) {
	view, metadata := parsedFixture(t, "claude", "claude-shell-command.jsonl")
	countIs(t, "user shell commands", metadata.Counts.UserShellCommands, 1)
	countIs(t, "turns", metadata.Counts.Turns, 1)
	countIs(t, "messages", metadata.Counts.Messages, 2)
	countIs(t, "tool calls", metadata.Counts.ToolCalls, 0)
	kinds := turnKinds(view)
	if kinds[0] != TurnKindShellCommand || kinds[1] != TurnKindCommandOutput || kinds[2] != TurnKindHumanPrompt {
		t.Fatalf("turn kinds = %#v", kinds)
	}
}

// A slash command stops being a candidate at the next thing the person did,
// and a trailing one with nothing after it is not a prompt. An isMeta record
// that kept content (a bundle from a harness that set the flag on something
// the filter cannot strip) is still never a prompt.
func TestParserV07SlashCommandResolution(t *testing.T) {
	now := time.Date(2026, 9, 22, 16, 0, 0, 0, time.UTC)
	user := func(content string) map[string]any {
		return map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}}
	}
	assistant := func(id string) map[string]any {
		return map[string]any{"type": "assistant", "message": map[string]any{"id": id, "role": "assistant", "content": "ok"}}
	}
	meta := user("expanded")
	meta["isMeta"] = true
	bundle := SourceBundle{
		SchemaVersion: 1, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p",
		Capture: SourceCapture{Harness: Harness{Name: "claude"}, AdapterName: "claude", AdapterVersion: adapterVersion, SourceFormat: "claude-jsonl", FilterVersion: FilterVersion, CapturedAt: now},
		NativeRecords: []map[string]any{
			user("<command-name>/clear</command-name>"), // 0: followed by a prompt
			user("hello"),   // 1
			assistant("m1"), // 2
			user("<command-name>/deploy</command-name>"), // 3: answered after meta and output
			meta, // 4
			user("<local-command-stdout>queued</local-command-stdout>"), // 5
			assistant("m2"), // 6
			user("<command-name>/cost</command-name>"), // 7: nothing after
		},
	}
	view, err := ParseNormalized(bundle)
	if err != nil {
		t.Fatal(err)
	}
	kinds := turnKinds(view)
	want := map[int]TurnKind{0: TurnKindLocalCommand, 1: TurnKindHumanPrompt, 3: TurnKindHumanPrompt, 4: TurnKindHarnessMeta, 5: TurnKindCommandOutput, 7: TurnKindLocalCommand}
	for index, kind := range want {
		if kinds[index] != kind {
			t.Errorf("record %d kind = %q, want %q", index, kinds[index], kind)
		}
	}
}

// harnessSession is one Claude session with every harness-written shape: a
// skill slash command and its expansion, /model with its caveat and output, a
// `!` command, and a streamed response. It is the input for the filter-3
// compatibility test below, written either as filter 3 left it (isMeta
// dropped, text kept) or as a native transcript for filter 4.
func harnessSession(withMetaFlags bool) []map[string]any {
	user := func(uuid, content string, meta bool) map[string]any {
		record := map[string]any{"type": "user", "uuid": uuid, "timestamp": "2026-09-22T17:00:00Z", "message": map[string]any{"role": "user", "content": content}}
		if meta && withMetaFlags {
			record["isMeta"] = true
		}
		return record
	}
	assistant := func(uuid, id string, block map[string]any) map[string]any {
		return map[string]any{"type": "assistant", "uuid": uuid, "timestamp": "2026-09-22T17:00:01Z", "message": map[string]any{"id": id, "role": "assistant", "model": "claude-opus-5", "content": []any{block}}}
	}
	return []map[string]any{
		user("r0", "<command-message>review-pr is running…</command-message>\n<command-name>/review-pr</command-name>\n<command-args>42</command-args>", false),
		user("r1", "Base directory for this skill: /work/skills/review-pr\n\nSynthetic skill instructions.", true),
		assistant("r2", "msg_a", map[string]any{"type": "text", "text": "Reviewing."}),
		user("r3", "<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args></command-args>", false),
		user("r4", "<local-command-caveat>Caveat: generated by local commands.</local-command-caveat>", true),
		user("r5", "<local-command-stdout>Set model to claude-opus-5</local-command-stdout>", false),
		user("r6", "<bash-input>ls</bash-input>", false),
		user("r7", "<bash-stdout>README.md</bash-stdout><bash-stderr></bash-stderr>", false),
		user("r8", "Fix the failing test.", false),
		assistant("r9", "msg_b", map[string]any{"type": "text", "text": "Looking."}),
		assistant("r10", "msg_b", map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Read", "input": map[string]any{"file_path": "/work/a_test.go"}}),
	}
}

// A bundle captured under filter 3 has no isMeta flag and still carries the
// expansion text, so parser 0.7 cannot recognize the expansion. It must still
// parse and regenerate, with counts no worse than parser 0.6 gave for it
// (0.6: 8 turns and 11 messages — every user record with text was a prompt and
// every streamed record a message) and never below the true figures. The same
// session captured under filter 4 must then give the same counts.
func TestParserV07RegeneratesFilterThreeBundlesNoWorse(t *testing.T) {
	now := time.Date(2026, 9, 22, 18, 0, 0, 0, time.UTC)
	reference := SourceReference{Key: "sessions/claude/a/source." + strings.Repeat("a", 64) + ".json.gz", SHA256: strings.Repeat("a", 64)}
	filterThree := SourceBundle{
		SchemaVersion: 1, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p",
		Capture:       SourceCapture{Harness: Harness{Name: "claude"}, AdapterName: "claude", AdapterVersion: "0.3.0", SourceFormat: "claude-jsonl", FilterVersion: "3", CapturedAt: now},
		NativeRecords: harnessSession(false),
	}
	old, err := BuildMetadata(filterThree, "m", now, now, reference, ParserInfo{})
	if err != nil {
		t.Fatalf("a filter 3 bundle no longer parses: %v", err)
	}
	if old.FilterVersion != "3" || old.Parser.Version != "0.7.0" {
		t.Fatalf("provenance = filter %q parser %q", old.FilterVersion, old.Parser.Version)
	}
	// Two things the person did were prompts (the /review-pr command and
	// "Fix the failing test."); filter 3 cannot tell the expansion from a
	// prompt, so it stands in for the command, and the total is still right.
	countIs(t, "filter 3 turns", old.Counts.Turns, 2)
	countIs(t, "filter 3 messages", old.Counts.Messages, 4)
	countIs(t, "filter 3 user shell commands", old.Counts.UserShellCommands, 1)
	countIs(t, "filter 3 tool calls", old.Counts.ToolCalls, 1)

	var lines []string
	for _, record := range harnessSession(true) {
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(encoded))
	}
	filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	reg := registration()
	reg.Harness = Harness{Name: "claude"}
	bundle, err := NewSourceBundle(reg, ClaudeAdapter{}, filtered, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	current, err := BuildMetadata(bundle, "m", now, now, reference, ParserInfo{})
	if err != nil {
		t.Fatal(err)
	}
	for label, pair := range map[string][2]*int{
		"turns": {old.Counts.Turns, current.Counts.Turns}, "messages": {old.Counts.Messages, current.Counts.Messages},
		"user shell commands": {old.Counts.UserShellCommands, current.Counts.UserShellCommands}, "tool calls": {old.Counts.ToolCalls, current.Counts.ToolCalls},
	} {
		if pair[0] == nil || pair[1] == nil || *pair[0] != *pair[1] {
			t.Errorf("%s: filter 3 = %v, filter 4 = %v", label, pair[0], pair[1])
		}
	}
	if strings.Contains(string(mustJSON(t, bundle)), "Synthetic skill instructions") {
		t.Fatal("filter 4 archived the skill expansion")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// Only a CommandExecution whose source is exactly unified_exec_startup is the
// startup shell. Another completed item type, or another source, is a tool
// call the model made.
func TestParserV07StartupShellExclusionIsExact(t *testing.T) {
	for _, item := range []map[string]any{
		{"type": "McpToolCall", "source": "unified_exec_startup", "id": "x"},
		{"type": "CommandExecution", "source": "UNIFIED_EXEC_STARTUP", "id": "x"},
		{"type": "CommandExecution", "source": "model", "id": "x"},
		{"type": "CommandExecution", "id": "x"},
	} {
		if isCodexStartupShell(item) {
			t.Errorf("excluded as the startup shell: %#v", item)
		}
	}
	if !isCodexStartupShell(map[string]any{"type": "CommandExecution", "source": "unified_exec_startup", "id": "x"}) {
		t.Error("the startup shell was not recognized")
	}
}

// A harness tag is recognized by prefix after leading whitespace, in string
// content or a text block alike; a prompt that merely mentions a tag is still
// a prompt.
func TestParserV07HarnessTagsAreRecognizedByPrefixOnly(t *testing.T) {
	now := time.Date(2026, 9, 22, 16, 0, 0, 0, time.UTC)
	user := func(content any) map[string]any {
		return map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}}
	}
	bundle := SourceBundle{
		SchemaVersion: 1, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p",
		Capture: SourceCapture{Harness: Harness{Name: "claude"}, AdapterName: "claude", AdapterVersion: adapterVersion, SourceFormat: "claude-jsonl", FilterVersion: FilterVersion, CapturedAt: now},
		NativeRecords: []map[string]any{
			user("  \n<bash-input>ls</bash-input>"),
			user([]any{map[string]any{"type": "text", "text": "\t<local-command-stdout>x</local-command-stdout>"}}),
			user("Why does <bash-input> appear in my transcript?"),
			user("Please explain the <command-name> tag"),
		},
	}
	view, err := ParseNormalized(bundle)
	if err != nil {
		t.Fatal(err)
	}
	kinds := turnKinds(view)
	want := map[int]TurnKind{0: TurnKindShellCommand, 1: TurnKindCommandOutput, 2: TurnKindHumanPrompt, 3: TurnKindHumanPrompt}
	for index, kind := range want {
		if kinds[index] != kind {
			t.Errorf("record %d kind = %q, want %q", index, kinds[index], kind)
		}
	}
}

// Filtering, bundling, and deriving metadata for each new fixture is
// deterministic: repeated scans produce byte-identical compressed bundles and
// metadata.
func TestParserV07FixturesScanDeterministically(t *testing.T) {
	now := time.Date(2026, 9, 22, 18, 0, 0, 0, time.UTC)
	for _, name := range []string{"claude-local-command.jsonl", "claude-shell-command.jsonl", "claude-skill-command.jsonl", "claude-streamed-response.jsonl", "codex-startup-shell.jsonl"} {
		harness := "claude"
		var adapter Adapter = ClaudeAdapter{}
		if strings.HasPrefix(name, "codex") {
			harness, adapter = "codex", CodexAdapter{}
		}
		var bundles, metadatas [][]byte
		for i := 0; i < 3; i++ {
			filtered, err := adapter.FilterJSONL(bytes.NewReader(fixture(t, name)))
			if err != nil {
				t.Fatal(err)
			}
			reg := registration()
			reg.Harness = Harness{Name: harness}
			bundle, err := NewSourceBundle(reg, adapter, filtered, now, nil)
			if err != nil {
				t.Fatal(err)
			}
			compressed, err := BuildCompressedSource(bundle)
			if err != nil {
				t.Fatal(err)
			}
			reference := SourceReference{Key: "sessions/" + harness + "/a/source." + compressed.SHA256 + ".json.gz", SHA256: compressed.SHA256}
			metadata, err := BuildMetadata(bundle, "m", now, now, reference, ParserInfo{})
			if err != nil {
				t.Fatal(err)
			}
			bundles, metadatas = append(bundles, compressed.Bytes), append(metadatas, mustJSON(t, metadata))
		}
		for i := 1; i < len(bundles); i++ {
			if !bytes.Equal(bundles[0], bundles[i]) {
				t.Errorf("%s: compressed bundle differs between scans", name)
			}
			if !bytes.Equal(metadatas[0], metadatas[i]) {
				t.Errorf("%s: metadata differs between scans", name)
			}
		}
	}
}
