# Antigravity CLI Integration Fixes (#748) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Resolve all P1 and P2 change requests from PR #748 by fixing Antigravity hook decoding, interactive handoff launching, privacy filter key retention, tool call/result linking, hook definition toggle handling, and complete end-to-end conformance testing.

**Architecture:** Connect the Antigravity CLI (`agy`) integration into the agent-archive lifecycle and discovery pipelines. Extend `hookconfig` to handle named definition boolean settings (`enabled`) and generate per-event command flags (`--event`); provide an Antigravity-specific decoder handling camelCase payloads and `SessionStart` freshness; switch the launcher to interactive `-i` with conflicting prompt flag rejection; expand the native privacy filter allowlist and tool argument retention with an atomic filter version bump (33 / 0.33.0); link tool arguments and execution results via `archive.FinalizeToolCalls`; and wire `NativeHeaders`, `Imports`, `RuntimeDetector`, `CapabilityEvidence`, and `VersionInspector` into `builtin.NewBuiltins()` with conformance and CLI golden updates.

**Tech Stack:** Go 1.24, standard library (`encoding/json`, `flag`, `os`, `path/filepath`, `strings`, `time`), internal packages (`agentapi`, `agentmeta`, `archive`, `discoveryio`, `hookconfig`, `importconfig`, `nativecodec`, `versionprobe`, `testutil/golden`).

**Spec:** Review comments from maintainer `wangjohn` on PR #748 (`https://github.com/Kaushikj-7/agent-archive/pull/748`) and Antigravity documentation (`https://antigravity.google/docs/hooks/`, `https://antigravity.google/docs/cli/reference/`).

## Global Constraints

- Go 1.24 standard library and existing internal packages only; no external dependencies.
- Architecture boundary rule: `internal/agents/agy` must never transitively import `cli`, `capture`, `collector`, or `backfill` (enforced by `TestAgentArchitectureBoundaries`).
- Privacy filter guarantee: Any change to filtered output requires incrementing `archive.FilterVersion` (32 -> 33) and `adapterVersion` (0.32.0 -> 0.33.0), and updating `versions.md` and `privacy-filter-changelog.md`.
- Conformance guarantee: All discovery agents in `builtin.NewBuiltins()` must implement `NativeHeaders`, `Imports`, and pass `TestDiscoveryConformance` and `TestHistoricalImportConformance`.
- Test suite guarantee: Every package containing tests must import `_ "github.com/wangjohn/agent-archive/internal/testutil/golden"` (enforced by `TestEveryTestedPackageKnowsUpdate`).
- Synthetic fixtures only: Any test data committed to git must be synthetic, never real developer credentials or private telemetry.

## Review Focus

1. **Hook definition `enabled: false` setting:** Antigravity hook files containing `{"agent-archive": {"enabled": false, ...}}` must not fail with "invalid hook list for enabled" during install, inspect, or remove, and remove must clean up the definition object when only `enabled` remains.
2. **Documented camelCase hook payloads without `hook_event_name`:** Payloads with `conversationId`, `transcriptPath`, and `workspacePaths` must decode without error via `--event <EventName>` and extract `workspacePaths[0]` as project root.
3. **Conflicting launch prompt flags:** Supplying `-p`, `--print`, `--prompt`, `-i`, or `--prompt-interactive` in `ExtraArgs` during handoff launch must return an explicit validation error rather than producing corrupted argv.
4. **Secret redaction inside Antigravity tool arguments:** A tool call record containing sensitive keys (e.g. `api_key: "secret"`) inside `toolCall.args` must have sensitive arguments stripped via `PrivacyDeniedArgument` while preserving `CommandLine`.
5. **Tool call and result linking in handoff:** Transcripts with tool calls (`RUN_COMMAND`) and tool results (`TOOL_RESULT`) must link inputs and results through `archive.FinalizeToolCalls` and render command line and output text in handoff output.

---

### Task 1: Handle Hook Definition `enabled` Setting in `hookconfig`

**Files:**
- Modify: `internal/agents/hookconfig/config.go:463-510` (`stripOwned`), `196-220` (`remove`), `113-148` (`merge`)
- Modify: `internal/agents/hookconfig/inspect.go:40-72` (`installed`), `85-113` (`otherInstallations`)
- Test: `internal/agents/hookconfig/config_test.go` (or `roundtrip_test.go`)
- Test: `internal/agents/agy/hooks_test.go:1-41`

**Interfaces:**
- Consumes: `hookconfig.Spec`, `hookconfig.Hook`, `*object`, `member`
- Produces: Correct handling of `m.Key == "enabled"` inside named definition objects (`spec.Definition != ""`) across `stripOwned`, `installed`, `otherInstallations`, and `remove`.

- [ ] **Step 1: Write the failing tests for `enabled` boolean setting**

Add test cases in `internal/agents/agy/hooks_test.go` and `internal/agents/hookconfig/roundtrip_test.go`:
```go
func TestHooksPreserveEnabledToggle(t *testing.T) {
	existing := []byte(`{
  "agent-archive": {
    "enabled": false,
    "PreInvocation": [
      {
        "type": "command",
        "command": "'/usr/local/bin/agent-archive' _hook --harness agy # agent-archive lifecycle capture",
        "timeout": "2"
      }
    ]
  }
}`)
	owner := agentapi.HookOwner{Executable: "/usr/local/bin/agent-archive", DataHome: "/tmp/archive"}
	inspection, err := Hooks().Inspect(agentapi.HookInspectionRequest{
		File:  agentapi.HookFile{Path: "/tmp/hooks.json", Bytes: existing, Present: true},
		Owner: owner,
	})
	if err != nil || !inspection.Installed {
		t.Fatalf("Inspect() on disabled hook = %#v, error = %v", inspection, err)
	}

	plan, err := Hooks().Plan(agentapi.HookPlanRequest{
		Action: agentapi.HookRemove,
		File:   agentapi.HookFile{Path: "/tmp/hooks.json", Bytes: existing, Present: true},
		Owner:  owner,
	})
	if err != nil {
		t.Fatalf("Plan(Remove) error = %v", err)
	}
	if strings.TrimSpace(string(plan[0].After)) != "{}" {
		t.Fatalf("remove left behind definition: %s", string(plan[0].After))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agents/agy -run TestHooksPreserveEnabledToggle -count=1`
Expected: FAIL with `invalid hook list for enabled`

- [ ] **Step 3: Implement `enabled` skipping and cleanup in `hookconfig`**

In `internal/agents/hookconfig/config.go`:
1. In `stripOwned`:
   ```go
   if app.Definition != "" && m.Key == "enabled" {
       if _, ok := m.Value.(bool); !ok {
           return nil, fmt.Errorf("invalid boolean for %s", m.Key)
       }
       continue
   }
   ```
2. In `remove`:
   ```go
   onlyEnabled := len(hs.Members) == 1 && hs.Members[0].Key == "enabled"
   if len(hs.Members) == 0 || onlyEnabled {
       if spec.Rootless {
           doc.Remove(spec.Definition)
           return doc.Bytes(), true, nil
       }
   ...
   ```
3. In `internal/agents/hookconfig/inspect.go`:
   In `installed` and `otherInstallations`:
   ```go
   if spec.Definition != "" && m.Key == "enabled" {
       if _, ok := m.Value.(bool); !ok {
           return false, fmt.Errorf("invalid boolean for %s", m.Key)
       }
       continue
   }
   ```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agents/agy ./internal/agents/hookconfig -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agents/hookconfig/config.go internal/agents/hookconfig/inspect.go internal/agents/agy/hooks_test.go
git commit -m "fix(hookconfig): handle enabled boolean setting in named hook definitions"
```

---

### Task 2: Per-Event Command Identification & CLI `_hook --event`

**Files:**
- Modify: `internal/agents/hookconfig/config.go:16-38` (`Spec`), `58-71` (`command`), `131-147` (`merge`)
- Modify: `internal/agents/hookconfig/inspect.go:10-73` (`installed`)
- Modify: `internal/agents/hookconfig/ports.go:98-102` (`Command`)
- Modify: `internal/cli/hook_command.go:60-70` (`runHookCommand`)
- Modify: `internal/cli/doc_commands_test.go:159-163`
- Test: `internal/agents/hookconfig/roundtrip_test.go`
- Test: `internal/cli/hook_command_test.go` (or `interactive_test.go`)

**Interfaces:**
- Consumes: `hookconfig.Spec.EventInCommand` bool
- Produces: `hook.commandForEvent(spec, event)` producing `_hook --harness <name> --event <event> # <owner>`, parsed by `_hook` CLI.

- [ ] **Step 1: Write the failing test for event in hook command**

In `internal/agents/hookconfig/ports_test.go`:
```go
func TestEventInCommandGeneratesEventFlag(t *testing.T) {
	spec := Spec{
		Name: "agy", EventInCommand: true, Owner: "test capture",
		Events: []string{"SessionStart", "Stop"}, Direct: true, Rootless: true, Definition: "agent-archive",
	}
	hook := Hook{Executable: "/bin/agent-archive"}
	cmdStart, err := hook.commandForEvent(spec, "SessionStart")
	if err != nil {
		t.Fatal(err)
	}
	want := "'/bin/agent-archive' _hook --harness agy --event SessionStart # test capture"
	if cmdStart != want {
		t.Fatalf("got %q, want %q", cmdStart, want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agents/hookconfig -run TestEventInCommandGeneratesEventFlag -count=1`
Expected: FAIL with `commandForEvent undefined`

- [ ] **Step 3: Implement `EventInCommand` and CLI `--event` flag**

1. In `internal/agents/hookconfig/config.go`:
   Add `EventInCommand bool` to `Spec`.
   Implement:
   ```go
   func (h Hook) commandForEvent(spec Spec, event string) (string, error) {
       if !spec.EventInCommand || event == "" {
           return h.command(spec)
       }
       if !filepath.IsAbs(h.Executable) || strings.ContainsAny(h.Executable, "\x00\r\n") {
           return "", errors.New("executable must be an absolute path")
       }
       command := quote(h.Executable) + " _hook --harness " + spec.Name + " --event " + event + " # " + spec.Owner
       if h.DataHome == "" {
           return command, nil
       }
       if !filepath.IsAbs(h.DataHome) || strings.ContainsAny(h.DataHome, "\x00\r\n") {
           return "", errors.New("data directory must be an absolute path")
       }
       return "AGENT_ARCHIVE_HOME=" + quote(h.DataHome) + " " + command, nil
   }
   ```
   In `merge`: use `command, err := hook.commandForEvent(spec, event)`.
2. In `internal/agents/hookconfig/inspect.go`:
   In `installed`: compute `expectedCommand, _ := hook.commandForEvent(app, m.Key)` and compare with `got`.
3. In `internal/cli/hook_command.go`:
   Add `eventFlag := fs.String("event", "", "hook event name")`.
   After decoding payload:
   ```go
   if *eventFlag != "" && payload != nil && payload["hook_event_name"] == nil {
       payload["hook_event_name"] = *eventFlag
   }
   ```
4. In `internal/cli/doc_commands_test.go`:
   Update `"_hook": {"harness", "event"}`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agents/hookconfig ./internal/cli -run "TestEventInCommand|TestDocumentedCommands" -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agents/hookconfig/ internal/cli/hook_command.go internal/cli/doc_commands_test.go
git commit -m "feat(hookconfig,cli): support per-event hook commands with --event flag"
```

---

### Task 3: Antigravity-Specific Hook Decoder & `SessionStart` Freshness

**Files:**
- Modify: `internal/agents/agy/decoder.go:1-20`
- Modify: `internal/agents/agy/hooks.go:1-17`
- Create: `internal/agents/agy/decoder_test.go`
- Modify: `internal/agents/agy/hooks_test.go`

**Interfaces:**
- Consumes: `agentapi.HookInput` with Antigravity camelCase JSON (`conversationId`, `transcriptPath`, `workspacePaths`, `modelName`, `invocationNum`)
- Produces: `agy.Decoder()` returning `agentapi.HookDecoder` & `agentapi.HookDiagnosticDecoder` mapping `SessionStart`, `PreInvocation`, `PostInvocation`, `Stop` to `LifecycleEvent`s with freshness distinction.

- [ ] **Step 1: Write failing tests for Antigravity hook decoding**

Create `internal/agents/agy/decoder_test.go`:
```go
package agy

import (
	"context"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestDecoderRecognizesDocumentedAntigravityPayload(t *testing.T) {
	d := Decoder()
	payload := map[string]any{
		"hook_event_name":   "SessionStart",
		"conversationId":    "12345678-1234-4234-8234-123456789abc",
		"transcriptPath":    "/home/user/.gemini/antigravity-cli/brain/12345678-1234-4234-8234-123456789abc/.system_generated/logs/transcript.jsonl",
		"workspacePaths":    []any{"/home/user/project"},
		"modelName":         "gemini-2.5-pro",
		"invocationNum":     float64(0),
	}
	events, err := d.Decode(context.Background(), agentapi.HookInput{Payload: payload, ObservedAt: time.Now()})
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(events) == 0 {
		t.Fatal("Decode() returned 0 events for documented SessionStart payload")
	}
	ev := events[0]
	if ev.Kind != agentapi.EventStart || ev.Session.NativeID != "12345678-1234-4234-8234-123456789abc" {
		t.Fatalf("unexpected event: %#v", ev)
	}
	if ev.ProjectRoot != "/home/user/project" {
		t.Fatalf("expected project root /home/user/project, got %q", ev.ProjectRoot)
	}
	if ev.Start.Kind != agentapi.FreshExplicit || ev.Start.Reason != agentapi.FreshnessExplicitStart {
		t.Fatalf("expected FreshExplicit for invocationNum 0, got %#v", ev.Start)
	}
}

func TestDecoderDistinguishesResumedSession(t *testing.T) {
	d := Decoder()
	payload := map[string]any{
		"hook_event_name": "SessionStart",
		"conversationId":  "12345678-1234-4234-8234-123456789abc",
		"transcriptPath":  "/path/to/transcript.jsonl",
		"workspacePaths":  []any{"/work"},
		"invocationNum":   float64(3),
	}
	events, err := d.Decode(context.Background(), agentapi.HookInput{Payload: payload, ObservedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if events[0].Start.Kind != agentapi.FreshContinuation {
		t.Fatalf("expected FreshContinuation for invocationNum 3, got %#v", events[0].Start)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agents/agy -run "TestDecoder" -count=1`
Expected: FAIL (returns 0 events because `conversationId` and `workspacePaths` are ignored by generic `hookconfig.Decoder`)

- [ ] **Step 3: Implement dedicated Antigravity `Decoder` and update `Hooks()`**

1. In `internal/agents/agy/decoder.go`:
   Implement `type Decoder struct{}`:
   - Extract session ID: `firstString(payload, "conversationId", "conversation_id", "session_id", "sessionId")`
   - Extract transcript path: `firstString(payload, "transcriptPath", "transcript_path")`
   - Extract project root: first string in `workspacePaths` array, or `cwd`
   - Event name: `firstString(payload, "hook_event_name", "event", "hookEventName")`
   - Map `SessionStart` -> `EventStart` (freshness check: `isResume == true || source == "resume" || invocationNum > 0` yields `FreshContinuation`; else `FreshExplicit`, `FreshnessExplicitStart`).
   - Map `PreInvocation` -> `EventTurnStart`
   - Map `PostInvocation` -> `EventResponse` with `archive.EvidenceKindFinalResponse` and `archive.EvidenceKindLifecycleHook`
   - Map `Stop` -> `EventStop` with `archive.EvidenceKindLifecycleHook`
   - Implement `DiagnosticProject(input agentapi.HookInput) string` extracting `workspacePaths[0]` or `cwd`.
2. In `internal/agents/agy/hooks.go`:
   Add `"SessionStart"` to `Events: []string{"SessionStart", "PreInvocation", "PostInvocation", "Stop"}`.
   Set `EventInCommand: true`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agents/agy -run "TestDecoder|TestHooks" -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agents/agy/decoder.go internal/agents/agy/decoder_test.go internal/agents/agy/hooks.go internal/agents/agy/hooks_test.go
git commit -m "feat(agy): implement Antigravity camelCase hook decoder and SessionStart freshness"
```

---

### Task 4: Interactive Handoff Launch with Conflicting Flag Rejection

**Files:**
- Modify: `internal/agents/agy/launch.go:18-29`
- Modify: `internal/agents/agy/launch_test.go:10-32`

**Interfaces:**
- Consumes: `agentapi.LaunchRequest`
- Produces: `Launcher.Args` returning `["--add-dir", <dir>, ..., "-i", <prompt>]`, rejecting `-p`, `--print`, `--prompt`, `-i`, `--prompt-interactive` in `r.ExtraArgs`.

- [ ] **Step 1: Write failing tests for interactive launch and flag conflicts**

In `internal/agents/agy/launch_test.go`:
```go
func TestLauncherArgsUsesInteractivePrompt(t *testing.T) {
	got, err := (Launcher{}).Args(agentapi.LaunchRequest{ProjectDir: `C:\work`, Prompt: "continue", ExtraArgs: []string{"--model", "gemini"}})
	if err != nil {
		t.Fatalf("Args() error = %v", err)
	}
	want := []string{"--add-dir", `C:\work`, "--model", "gemini", "-i", "continue"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Args() = %#v, want %#v", got, want)
	}
}

func TestLauncherRejectsAllConflictingPromptFlags(t *testing.T) {
	for _, flag := range []string{"-p", "--prompt", "--print", "-i", "--prompt-interactive"} {
		if _, err := (Launcher{}).Args(agentapi.LaunchRequest{ExtraArgs: []string{flag}, Prompt: "handoff"}); err == nil {
			t.Fatalf("Args() accepted conflicting flag %q", flag)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agents/agy -run "TestLauncher" -count=1`
Expected: FAIL (Args returns `-p` instead of `-i`, and does not reject `--print` or `--prompt-interactive`)

- [ ] **Step 3: Implement `-i` default and prompt flag validation in `launch.go`**

In `internal/agents/agy/launch.go`:
```go
var conflictingPromptFlags = []string{"-p", "--prompt", "--print", "-i", "--prompt-interactive"}

func (Launcher) Args(r agentapi.LaunchRequest) ([]string, error) {
	for _, flag := range conflictingPromptFlags {
		if slices.Contains(r.ExtraArgs, flag) {
			return nil, fmt.Errorf("arguments for agy cannot include %s: the handoff prompt is managed by agent-archive", flag)
		}
	}
	args := slices.Clone(r.ExtraArgs)
	if r.ProjectDir != "" {
		args = append([]string{"--add-dir", r.ProjectDir}, args...)
	}
	args = append(args, "-i", r.Prompt)
	return args, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agents/agy -run "TestLauncher" -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agents/agy/launch.go internal/agents/agy/launch_test.go
git commit -m "fix(agy): default handoff to interactive prompt -i and reject conflicting prompt flags"
```

---

### Task 5: Privacy Filter Safe Keys, Tool Argument Retention & Version Bump

**Files:**
- Modify: `internal/archive/types.go:52` (bump `FilterVersion = "33"`)
- Modify: `internal/agents/nativecodec/adapters.go:758` (bump `adapterVersion = "0.33.0"`)
- Modify: `internal/agents/nativecodec/adapters.go:108` (`AgyAdapter.Version` returns `adapterVersion`)
- Modify: `internal/agents/nativecodec/adapters.go:785, 799-801` (`noteNativeIdentity`, `noteSafeIdentity`)
- Modify: `internal/agents/nativecodec/privacy.go:11-66` (`allowedKeys`)
- Modify: `internal/agents/nativecodec/semantics.go:551` (`toolArgumentKeys`)
- Modify: `dev/maintainers/versions.md`
- Modify: `dev/specs/privacy-filter-changelog.md`
- Modify: `dev/specs/privacy-filter.md`
- Test: `internal/agents/agy/filter_test.go` (or `parser_test.go`)

**Interfaces:**
- Consumes: Raw Antigravity JSONL records with `modelName`, `step_id`, `conversationId`, `prompt`, `toolCall`, and `args`
- Produces: `FilterJSONL` retaining structural fields and non-sensitive tool arguments while redacting secrets and bumping `FilterVersion` to `"33"`.

- [ ] **Step 1: Write failing test for filtering and parsing together**

In `internal/agents/agy/parser_test.go`:
```go
func TestFilterPreservesAntigravityParserFieldsAndRedactsSecrets(t *testing.T) {
	raw := `{"type":"USER_INPUT","prompt":"<USER_REQUEST>run tests</USER_REQUEST>","conversationId":"conv-123","step_id":"step-1","created_at":"2026-10-26T10:00:00Z"}
{"type":"PLANNER_RESPONSE","content":"Running","modelName":"gemini-2.5-pro","created_at":"2026-10-26T10:00:01Z"}
{"type":"RUN_COMMAND","toolCall":{"name":"run_command","args":{"CommandLine":"go test ./...","api_key":"supersecret"}},"step_id":"step-2","created_at":"2026-10-26T10:00:02Z"}
`
	filtered, err := (Filter{}).FilterJSONL(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("FilterJSONL() error = %v", err)
	}
	filteredStr := string(bytes.Join(filtered.Records, []byte("\n")))
	if strings.Contains(filteredStr, "supersecret") {
		t.Fatal("api_key secret was not redacted")
	}
	if !strings.Contains(filteredStr, "go test ./...") {
		t.Fatal("CommandLine was dropped by privacy filter")
	}
	if !strings.Contains(filteredStr, "gemini-2.5-pro") {
		t.Fatal("modelName was dropped by privacy filter")
	}
	if !strings.Contains(filteredStr, "conv-123") {
		t.Fatal("conversationId was dropped by privacy filter")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agents/agy -run TestFilterPreservesAntigravityParserFieldsAndRedactsSecrets -count=1`
Expected: FAIL (modelName, conversationId, or CommandLine dropped)

- [ ] **Step 3: Update `allowedKeys`, `toolArgumentKeys`, identity notes, and versions**

1. In `internal/agents/nativecodec/privacy.go`:
   Add to `allowedKeys`:
   `"modelname": true, "model_name": true, "step_id": true, "conversationid": true, "conversation_id": true, "prompt": true, "toolcall": true, "args": true`
2. In `internal/agents/nativecodec/semantics.go`:
   Update `toolArgumentKeys = map[string]bool{"input": true, "arguments": true, "tool_input": true, "args": true, "toolcall": true}`
3. In `internal/agents/nativecodec/adapters.go`:
   - In `noteNativeIdentity`: include `"conversationId", "conversation_id"` in `firstString`.
   - In `noteSafeIdentity`: include `"conversationId", "conversation_id"` in `LocalIdentity.Candidates`.
   - Change `const adapterVersion = "0.33.0"`.
   - In `AgyAdapter.Version()`: return `adapterVersion`.
4. In `internal/archive/types.go`:
   Change `FilterVersion = "33"`.
5. Update `dev/maintainers/versions.md` table and notes.
6. Add entry to `dev/specs/privacy-filter-changelog.md` for Filter version 33.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agents/agy ./internal/agents/nativecodec -run "TestFilter" -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/archive/types.go internal/agents/nativecodec/ dev/maintainers/versions.md dev/specs/privacy-filter-changelog.md internal/agents/agy/parser_test.go
git commit -m "feat(nativecodec,archive): bump FilterVersion 33 and retain safe Antigravity parser keys"
```

---

### Task 6: Parser Tool Arguments, Execution Details & Results Linking

**Files:**
- Modify: `internal/agents/agy/parser.go:48-93`
- Modify: `internal/agents/agy/parser_test.go`

**Interfaces:**
- Consumes: Filtered `archive.SourceBundle` with `RUN_COMMAND` / `TOOL_CALL` and `TOOL_RESULT`
- Produces: Populated `archive.NormalizedToolCall` with `Input`, `ShellCommand`, `Action`, `ResultText`, linked via `archive.FinalizeToolCalls`, and evidence-based `analysis.Observability.ToolResults`.

- [ ] **Step 1: Write failing test for tool argument and result linking in handoff**

In `internal/agents/agy/parser_test.go`:
```go
func TestParserPopulatesToolDetailsAndLinksResults(t *testing.T) {
	bundle := archive.SourceBundle{
		NativeSessionID: "conv-1",
		NativeRecords: []map[string]any{
			{"type": "USER_INPUT", "content": "run tests", "created_at": "2026-10-26T10:00:00Z"},
			{
				"type": "RUN_COMMAND",
				"id":   "call-1",
				"toolCall": map[string]any{
					"name": "run_command",
					"args": map[string]any{"CommandLine": "go test ./..."},
				},
				"created_at": "2026-10-26T10:00:01Z",
			},
			{
				"type":       "TOOL_RESULT",
				"id":         "call-1",
				"content":    "FAIL: tests failed",
				"is_error":   true,
				"created_at": "2026-10-26T10:00:02Z",
			},
		},
	}
	analysis, err := (Parser{}).Parse(context.Background(), bundle)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(analysis.View.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(analysis.View.ToolCalls))
	}
	call := analysis.View.ToolCalls[0]
	if call.ShellCommand != "go test ./..." {
		t.Fatalf("expected ShellCommand 'go test ./...', got %q", call.ShellCommand)
	}
	if call.Action.Kind != archive.ToolActionShell {
		t.Fatalf("expected ToolActionShell, got %v", call.Action.Kind)
	}
	if call.IsError == nil || !*call.IsError {
		t.Fatalf("expected IsError=true linked to call, got %v", call.IsError)
	}
	if call.ResultText != "FAIL: tests failed" {
		t.Fatalf("expected ResultText 'FAIL: tests failed', got %q", call.ResultText)
	}
	if analysis.Observability.ToolResults.State != archive.AvailabilityAvailable {
		t.Fatalf("expected ToolResults Available when results exist, got %v", analysis.Observability.ToolResults.State)
	}
}

func TestParserToolResultAvailabilityUnavailableWhenNoResultsRecorded(t *testing.T) {
	bundle := archive.SourceBundle{
		NativeSessionID: "conv-2",
		NativeRecords: []map[string]any{
			{"type": "USER_INPUT", "content": "run tests", "created_at": "2026-10-26T10:00:00Z"},
			{
				"type": "RUN_COMMAND",
				"id":   "call-1",
				"toolCall": map[string]any{
					"name": "run_command",
					"args": map[string]any{"CommandLine": "go test ./..."},
				},
				"created_at": "2026-10-26T10:00:01Z",
			},
		},
	}
	analysis, err := (Parser{}).Parse(context.Background(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	if analysis.Observability.ToolResults.State != archive.AvailabilityUnavailable {
		t.Fatalf("expected ToolResults Unavailable when no results recorded, got %v", analysis.Observability.ToolResults.State)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agents/agy -run "TestParserPopulatesToolDetails|TestParserToolResultAvailability" -count=1`
Expected: FAIL (ShellCommand empty, results unlinked, or Observability incorrect)

- [ ] **Step 3: Implement tool extraction, action building, and result linking in `parser.go`**

In `internal/agents/agy/parser.go`:
1. Collect tool call candidates:
   ```go
   var candidates []archive.ToolCallCandidate
   ```
2. In `case "RUN_COMMAND", "TOOL_CALL":`:
   - Extract `args := toolArguments(record)`
   - Extract `name := toolName(record)`
   - Build `archive.ToolAction`:
     - If `name` is `run_command`, `bash`, `exec_command`: `Action.Kind = archive.ToolActionShell`, `Action.Text = firstString(args, "CommandLine", "command", "cmd")`, `ShellCommand = Action.Text`
     - If `name` is `view_file`, `read_file`: `Action.Kind = archive.ToolActionRead`, `Action.Path = firstString(args, "AbsolutePath", "path", "file_path")`
     - If `name` is `write_to_file`, `replace_file_content`: `Action.Kind = archive.ToolActionEdit`, `Action.Files = []string{firstString(args, "TargetFile", "target_file", "path")}`
     - If `name` is `grep_search`: `Action.Kind = archive.ToolActionSearch`, `Action.Text = firstString(args, "Query", "query")`
   - Build `NormalizedToolCall{RecordIndex: index, CallID: id, Name: name, Model: model, Input: args, Action: action, ShellCommand: shellCmd}`
   - Append to `candidates = append(candidates, archive.ToolCallCandidate{Call: call})`
3. In `case "TOOL_RESULT":`:
   - Extract output text: `text, _ := recordText(record)`
   - Extract `isError`: `isError, _ := record["is_error"].(bool)` (also check `record["isError"]`, or status `"ERROR"`)
   - `view.ToolResults = append(view.ToolResults, archive.NormalizedToolResult{RecordIndex: index, CallID: id, IsError: isError, OutputBytes: len(text), Text: text})`
4. After loop:
   ```go
   view.ToolCalls = archive.FinalizeToolCalls(candidates, view.ToolResults)
   ```
   Set `analysis.Observability.ToolResults`:
   ```go
   toolResultsAvail := archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonNotRecorded}
   if len(view.ToolResults) > 0 {
       toolResultsAvail = structured
   }
   analysis.Observability.ToolResults = toolResultsAvail
   ```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agents/agy -run "TestParser" -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agents/agy/parser.go internal/agents/agy/parser_test.go
git commit -m "feat(agy): extract tool arguments and actions, link results, and base availability on evidence"
```

---

### Task 7: Native Headers, Historical Imports, Runtime Detection & Capability Evidence

**Files:**
- Create: `internal/agents/agy/native_headers.go`
- Create: `internal/agents/agy/import.go`
- Create: `internal/agents/agy/runtime.go`
- Create: `internal/agents/agy/capabilities.go`
- Create: `internal/agents/agy/version.go`
- Modify: `internal/agents/builtin/registry.go:143`
- Test: `internal/agents/agy/native_headers_test.go`
- Test: `internal/agents/agy/runtime_test.go`
- Test: `internal/agents/builtin/agy_test.go`

**Interfaces:**
- Consumes: `agentapi.NativeHeaderInspector`, `agentapi.ImportInspector`, `agentapi.RuntimeDetector`, `agentapi.CapabilityEvidenceProvider`, `agentapi.VersionInspector`
- Produces: Full port bindings on `agentmeta.Agy` in `builtin.NewBuiltins()`.

- [ ] **Step 1: Write failing tests for NativeHeaders, Imports, and Runtime on Agy**

In `internal/agents/builtin/agy_test.go`:
```go
func TestAgyPortsRegisteredInBuiltins(t *testing.T) {
	r := NewBuiltins()
	if _, ok := r.LookupNativeHeaders("agy"); !ok {
		t.Fatal("Agy NativeHeaders not registered")
	}
	if _, ok := r.LookupImport("agy"); !ok {
		t.Fatal("Agy Imports not registered")
	}
	if _, ok := r.LookupCapabilityEvidence("agy"); !ok {
		t.Fatal("Agy CapabilityEvidence not registered")
	}
	if _, ok := r.LookupVersionInspector("agy"); !ok {
		t.Fatal("Agy VersionInspector not registered")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agents/builtin -run TestAgyPortsRegisteredInBuiltins -count=1`
Expected: FAIL (missing NativeHeaders / Imports)

- [ ] **Step 3: Implement `NativeHeaders`, `Imports`, `RuntimeDetector`, `CapabilityEvidence`, and `VersionInspector`**

1. `internal/agents/agy/native_headers.go`:
   - Implement `NativeHeaders` with `InspectHeader`, `Roots`, `Discover`, `DefaultDirectories`.
   - `Roots`: scans `filepath.Join(dir, "brain")` with `Suffix: "transcript.jsonl"`.
   - `InspectHeader`: extracts conversation ID from parent directory name (verifying with `validConversationID`), scans for `cwd` and start timestamp.
   - `Discover`: calls `discoveryio.Files(ctx, r, agentmeta.Agy, roots, p, false, emit)`.
2. `internal/agents/agy/import.go`:
   - `func Imports() agentapi.ImportInspector`: returns `importconfig.Provider{RequireSessionMembership: true, NativeStart: true, ConversationTypes: []string{"USER_INPUT", "PLANNER_RESPONSE", "user", "assistant", "message", "response_item", "tool_use", "tool_result", "tool_call"}}`.
3. `internal/agents/agy/runtime.go`:
   - `type RuntimeDetector struct{}`
   - `Detect`: looks up `ANTIGRAVITY_SESSION_ID`, `AGY_SESSION_ID`, `ANTIGRAVITY_CONVERSATION_ID`. Fallback to `ANTIGRAVITY_AGENT`, `AGY_AGENT`, `ANTIGRAVITY_CLI`.
   - `SessionEnvironmentKeys`: returns session keys to strip on launch.
4. `internal/agents/agy/capabilities.go`:
   - `type CapabilityEvidence struct{}`
   - `CaptureEvidence() agentapi.CaptureCapabilities`: declares documented transcript and lifecycle hooks, unavailable skill evidence.
5. `internal/agents/agy/version.go`:
   - `type VersionInspector struct{}`
   - `Candidates`: returns `"agy"`, `"agy.exe"`, and standard user paths.
   - `ObserveVersion`: calls `versionprobe.Commands(e.Host, "agy", p.Candidates(e))`.
6. In `internal/agents/builtin/registry.go:143`:
   Bind all ports to `agentmeta.Agy`:
   ```go
   {Descriptor: agentmeta.Descriptor{ID: agentmeta.Agy}, Launcher: agy.Launcher{}, Sources: agy.SourceProvider{}, Filter: agy.Filter{}, Parser: agy.Parser{}, Discovery: agy.NativeHeaders{}, NativeHeaders: agy.NativeHeaders{}, Imports: agy.Imports(), Runtime: agy.RuntimeDetector{}, Hooks: agy.Hooks(), Decoder: agy.Decoder(), Evidence: agy.CapabilityEvidence{}, Version: agy.VersionInspector{}},
   ```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agents/builtin -run TestAgyPortsRegisteredInBuiltins -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agents/agy/ internal/agents/builtin/registry.go internal/agents/builtin/agy_test.go
git commit -m "feat(agy): implement NativeHeaders, Imports, Runtime, Evidence and bind in registry"
```

---

### Task 8: Conformance Tests, Golden Updates, CLI Fixtures, and Documentation

**Files:**
- Create: `internal/agents/agy/main_test.go`
- Modify: `internal/agents/builtin/discovery_test.go:13-17` (`fixtures`)
- Modify: `internal/agents/builtin/import_test.go:33-43`
- Modify: `internal/cli/muse_runtime_test.go:61`
- Modify: `internal/cli/capabilities_test.go:23`
- Modify: `internal/cli/setup_prompts_test.go:30-45`
- Modify: `internal/cli/handoff_destination_test.go:86-105`
- Modify: `README.md`, `docs/`, `CHANGELOG.md`
- Test: All package tests across `./internal/agents/agy/...`, `./internal/agents/builtin/...`, `./internal/cli/...`

**Interfaces:**
- Consumes: All updated providers and catalog entries
- Produces: 100% passing tests across conformance, architecture boundaries, update-flag registration, and CLI interactive suites.

- [ ] **Step 1: Add `main_test.go` in `internal/agents/agy/`**

Create `internal/agents/agy/main_test.go`:
```go
package agy

import _ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
```

- [ ] **Step 2: Add `agy` fixtures in conformance tests**

1. In `internal/agents/builtin/discovery_test.go`:
   Add to `fixtures`:
   ```go
   "agy": {
       ".gemini/antigravity-cli/brain/11111111-1111-4111-8111-111111111111/.system_generated/logs/transcript.jsonl",
       ".gemini/antigravity-cli/brain/22222222-2222-4222-8222-222222222222/.system_generated/logs/transcript.jsonl",
   },
   ```
2. In `internal/agents/builtin/import_test.go`:
   In `switch agentmeta.ID(name)`:
   ```go
   case agentmeta.Agy:
       expected.StartedAt = nativeStart
   ```
   and update mismatch expectation:
   ```go
   if name == "claude" || name == "agy" {
       mismatch.IdentityMismatch = true
   }
   ```

- [ ] **Step 3: Update CLI tests and expectations for the 4-harness set**

1. In `internal/cli/muse_runtime_test.go:61`:
   Update `[]string{"codex", "claude", "cursor"}` -> `[]string{"codex", "claude", "cursor", "agy"}`.
2. In `internal/cli/capabilities_test.go:23`:
   Include `"agy"` in capability profile checks.
3. In `internal/cli/setup_prompts_test.go`:
   Update test cases with full harnesses to reflect the 4-harness set.
4. In `internal/cli/handoff_destination_test.go`:
   Update destination choices test cases.
5. Run `go test ./internal/cli/... -update` if any golden files need refreshing, and review git diff carefully.

- [ ] **Step 4: Update Documentation and Changelog**

1. In `README.md`: document Antigravity CLI (`agy`) support for capture, browsing, backfill, and handoff.
2. In `CHANGELOG.md`: add entry for Antigravity integration fixes and FilterVersion 33 / adapterVersion 0.33.0.
3. In user docs under `docs/`: describe `agy` CLI handoff, hook configuration, and discovery roots.

- [ ] **Step 5: Run full verification suite**

Run:
```bash
go test -cover ./internal/agents/agy/... -count=1
go test ./internal/agents/builtin/... -count=1
go test ./internal/agents/hookconfig/... -count=1
go test ./internal/agents/nativecodec/... -count=1
go test ./internal/cli/... -count=1
go vet ./internal/agents/agy ./internal/agents/builtin ./internal/agents/hookconfig ./internal/agents/nativecodec ./internal/cli
```
Expected: All tests PASS with zero regressions.

- [ ] **Step 6: Commit**

```bash
git add internal/agents/agy/main_test.go internal/agents/builtin/discovery_test.go internal/agents/builtin/import_test.go internal/cli/ README.md CHANGELOG.md docs/
git commit -m "test(conformance,cli): add agy conformance fixtures, update test suites and documentation"
```
