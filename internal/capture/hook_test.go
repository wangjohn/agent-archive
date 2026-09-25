package capture

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestAmbiguousStartIsNotRegisteredAndDiagnosticIsContentFree(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	// Both starts name a file that already holds a conversation, so neither is
	// provably the beginning of one, whatever the harness reports.
	resumed := writeTestTranscript(t, "resumed.jsonl", `{"type":"user"}`)
	for _, tc := range []struct {
		harness string
		payload map[string]any
	}{
		{"codex", map[string]any{"hook_event_name": "SessionStart", "session_id": "private-codex-id", "cwd": "/work/widget", "transcript_path": resumed}},
		{"cursor", map[string]any{"hook_event_name": "sessionStart", "conversation_id": "private-cursor-id", "workspace_roots": []any{"/work/widget"}, "transcript_path": resumed}},
	} {
		if err := handleHookEvent(home, tc.harness, tc.payload, now); err != nil {
			t.Fatal(err)
		}
	}
	store, _ := state.Open(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 0 {
		t.Fatalf("ambiguous starts were registered: %#v", regs)
	}
	diagnostics, err := readCaptureDiagnostics(home)
	if err != nil || len(diagnostics) != 2 {
		t.Fatalf("diagnostics=%#v err=%v", diagnostics, err)
	}
	raw, err := os.ReadFile(captureDiagnosticsPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) || bytes.Contains(raw, []byte("private-")) || bytes.Contains(raw, []byte("transcript")) {
		t.Fatalf("diagnostic leaked session content or identity: %s", raw)
	}
}

func setUpTestConfig(t *testing.T, home, projectRoot string, activatedAt time.Time) {
	t.Helper()
	cfg := config.Config{
		MachineID: "machine-1",
		Storage:   credentialsTestConfig(),
		Archive: archive.Config{
			SchemaVersion: 1, MachineID: "machine-1", Enabled: true,
			Projects: []archive.ProjectActivation{{ProjectID: archive.ProjectID(projectRoot), Root: projectRoot, Included: true, ActivatedAt: activatedAt}},
		},
	}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyHookEvent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		harness string
		event   string
		want    hookEventKind
	}{
		{"codex", "SessionStart", hookEventStart},
		{"codex", "Interrupt", hookEventStop},
		{"codex", "SubagentStop", hookEventSubagentStop},
		{"codex", "UserPromptSubmit", hookEventTurnStart},
		{"claude", "StopFailure", hookEventStop},
		{"claude", "PreToolUse", hookEventIgnored},
		{"cursor", "sessionStart", hookEventStart},
		{"cursor", "beforeSubmitPrompt", hookEventTurnStart},
		{"cursor", "afterAgentResponse", hookEventResponse},
		{"cursor", "stop", hookEventStop},
		{"cursor", "SessionStart", hookEventIgnored}, // wrong case for this harness
	}
	for _, c := range cases {
		if got := classifyHookEvent(c.harness, c.event); got != c.want {
			t.Errorf("classifyHookEvent(%q, %q) = %v, want %v", c.harness, c.event, got, c.want)
		}
	}
}

func TestHandleHookEventRegistersEligibleSessionStart(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	payload := map[string]any{
		"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1",
		"cwd": "/work/widget", "transcript_path": "/tmp/t.jsonl",
	}
	if err := handleHookEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}

	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) != 1 {
		t.Fatalf("regs=%#v", regs)
	}
	if regs[0].NativeSessionID != "native-1" || regs[0].ProjectRoot != "/work/widget" || !regs[0].SessionStartedAt.Equal(now) {
		t.Fatalf("reg=%#v", regs[0])
	}
}

func TestHandleHookEventSkipsIneligibleProject(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": "/somewhere/else"}
	if err := handleHookEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 0 {
		t.Fatalf("regs=%#v", regs)
	}
}

func TestHandleHookEventSkipsResumeOfUnknownClaudeSession(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	payload := map[string]any{
		"hook_event_name": "SessionStart", "session_id": "native-old", "source": "resume",
		"cwd": "/work/widget",
	}
	if err := handleHookEvent(home, "claude", payload, now); err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 0 {
		t.Fatalf("a resume of a never-seen session must not be registered: %#v", regs)
	}
}

func TestHandleHookEventSkipsResumeOfUnknownCodexSession(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	for _, source := range []string{"resume", "compact"} {
		payload := map[string]any{
			"hook_event_name": "SessionStart", "session_id": "native-old-" + source, "source": source,
			"cwd": "/work/widget",
		}
		if err := handleHookEvent(home, "codex", payload, now); err != nil {
			t.Fatal(err)
		}
	}
	store, _ := state.Open(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 0 {
		t.Fatalf("a Codex resume/compact of a never-seen session must not be registered: %#v", regs)
	}
}

func TestHandleHookEventRegistersCodexStartup(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	for _, source := range []string{"startup", "clear"} {
		payload := map[string]any{
			"hook_event_name": "SessionStart", "session_id": "native-" + source, "source": source,
			"cwd": "/work/widget", "transcript_path": "/tmp/t.jsonl",
		}
		if err := handleHookEvent(home, "codex", payload, now); err != nil {
			t.Fatal(err)
		}
	}
	store, _ := state.Open(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 2 {
		t.Fatalf("a Codex startup/clear must register a fresh session: %#v", regs)
	}
	for _, reg := range regs {
		if reg.Harness.Name != "codex" || !reg.SessionStartedAt.Equal(now) {
			t.Fatalf("reg=%#v", reg)
		}
	}
}

func TestHandleHookEventCodexCompactPreservesOriginalStartTime(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	firstStart := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{"hook_event_name": "SessionStart", "session_id": "native-1", "source": "startup", "cwd": "/work/widget"}
	if err := handleHookEvent(home, "codex", payload, firstStart); err != nil {
		t.Fatal(err)
	}

	compactAt := firstStart.Add(2 * time.Hour)
	compactPayload := map[string]any{"hook_event_name": "SessionStart", "session_id": "native-1", "source": "compact", "cwd": "/work/widget"}
	if err := handleHookEvent(home, "codex", compactPayload, compactAt); err != nil {
		t.Fatal(err)
	}

	store, _ := state.Open(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 1 {
		t.Fatalf("regs=%#v", regs)
	}
	if !regs[0].SessionStartedAt.Equal(firstStart) {
		t.Fatalf("compact must preserve the original start time: got %s want %s", regs[0].SessionStartedAt, firstStart)
	}
}

func TestHandleHookEventResumePreservesOriginalStartTime(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	firstStart := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": "/work/widget", "transcript_path": "/tmp/t.jsonl"}
	if err := handleHookEvent(home, "claude", payload, firstStart); err != nil {
		t.Fatal(err)
	}

	resumeAt := firstStart.Add(48 * time.Hour)
	resumePayload := map[string]any{
		"hook_event_name": "SessionStart", "session_id": "native-1", "source": "resume",
		"cwd": "/work/widget", "transcript_path": "/tmp/t2.jsonl",
	}
	if err := handleHookEvent(home, "claude", resumePayload, resumeAt); err != nil {
		t.Fatal(err)
	}

	store, _ := state.Open(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 1 {
		t.Fatalf("regs=%#v", regs)
	}
	if !regs[0].SessionStartedAt.Equal(firstStart) {
		t.Fatalf("resume must preserve the original start time: got %s want %s", regs[0].SessionStartedAt, firstStart)
	}
	if regs[0].TranscriptPath != "/tmp/t2.jsonl" {
		t.Fatalf("resume should refresh the transcript path: %#v", regs[0])
	}
}

func TestHandleHookEventStopWritesRequestWithEvidence(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	startPayload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": "/work/widget"}
	if err := handleHookEvent(home, "claude", startPayload, start); err != nil {
		t.Fatal(err)
	}

	stopAt := start.Add(time.Minute)
	stopPayload := map[string]any{"hook_event_name": "Stop", "session_id": "native-1", "turn_id": "t1", "model": "claude-opus-5"}
	if err := handleHookEvent(home, "claude", stopPayload, stopAt); err != nil {
		t.Fatal(err)
	}
	if err := handleHookEvent(home, "claude", map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "native-1"}, stopAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	store, _ := state.Open(home)
	requests, err := store.LoadRequests()
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || !slices.Contains(requests[0].Reasons, "stop") || !slices.Contains(requests[0].Reasons, "userpromptsubmit") {
		t.Fatalf("requests=%#v", requests)
	}
	if len(requests[0].HookEvidence) != 4 {
		t.Fatalf("evidence=%#v", requests[0].HookEvidence)
	}
	var lifecycle, resumed, final archive.SupplementalEvidence
	for _, item := range requests[0].HookEvidence {
		if item.Kind == archive.EvidenceKindLifecycleHook && item.Payload["event_name"] == "Stop" {
			lifecycle = item
		}
		if item.Kind == archive.EvidenceKindLifecycleHook && item.Payload["event_name"] == "UserPromptSubmit" {
			resumed = item
		}
		if item.Kind == archive.EvidenceKindFinalResponse {
			final = item
		}
	}
	if lifecycle.Payload["event_name"] != "Stop" || resumed.Payload["event_name"] != "UserPromptSubmit" || final.Payload["turn_id"] != "t1" {
		t.Fatalf("lifecycle=%#v resumed=%#v final=%#v", lifecycle, resumed, final)
	}
}

func TestHandleHookEventCapturesSupportedFinalTextAfterFiltering(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	if err := handleHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": "/work/widget"}, start); err != nil {
		t.Fatal(err)
	}
	stop := map[string]any{"hook_event_name": "Stop", "session_id": "native-1", "turn_id": "t1", "model": "gpt-x", "last_assistant_message": "done token=synthetic-secret-value"}
	if err := handleHookEvent(home, "codex", stop, start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(home)
	requests, err := store.LoadRequests()
	if err != nil {
		t.Fatal(err)
	}
	var final archive.SupplementalEvidence
	for _, item := range requests[0].HookEvidence {
		if item.Kind == archive.EvidenceKindFinalResponse {
			final = item
		}
	}
	if text, _ := final.Payload["text"].(string); final.Payload["redacted"] != true || strings.Contains(text, "synthetic-secret-value") || !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("final evidence was not filtered before persistence: %#v", final)
	}
}

func TestHandleHookEventLabelsSubagentFinalGapWithoutClaimingRedaction(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	if err := handleHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": "/work/widget"}, start); err != nil {
		t.Fatal(err)
	}
	stop := map[string]any{"hook_event_name": "Stop", "session_id": "native-1", "turn_id": "t1", "agent_id": "sub-1", "last_assistant_message": "plain final text"}
	if err := handleHookEvent(home, "codex", stop, start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(home)
	requests, err := store.LoadRequests()
	if err != nil {
		t.Fatal(err)
	}
	var final archive.SupplementalEvidence
	for _, item := range requests[0].HookEvidence {
		if item.Kind == archive.EvidenceKindFinalResponse {
			final = item
		}
	}
	if final.Payload["text"] != "plain final text" || final.Payload["redacted"] != nil || final.Payload["truncated"] != nil {
		t.Fatalf("unredacted final was mislabeled: %#v", final)
	}
	if gaps, _ := final.Payload["gaps"].([]any); len(gaps) != 1 || gaps[0] != "subagent_final_not_reconciled" {
		t.Fatalf("producer gap was not carried on the evidence: %#v", final)
	}
}

func TestAcceptedCursorHookCapturesVersionModeModelParamsAndResponse(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	store, _ := state.Open(home)
	archiveID, _, err := store.EnsureArchiveSessionID("native-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRegistration(archive.SessionRegistration{ArchiveSessionID: archiveID, NativeSessionID: "native-1", ProjectID: "synthetic-project", ProjectRoot: "/work/widget", Harness: archive.Harness{Name: "cursor"}, SessionStartedAt: now, RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	start := map[string]any{"hook_event_name": "sessionStart", "conversation_id": "native-1", "workspace_roots": []any{"/work/widget"}, "cursor_version": "1.7.2", "composer_mode": "agent", "model": "label", "model_id": "model-x", "model_params": []any{map[string]any{"id": "effort", "value": "high"}}}
	if err := handleHookEvent(home, "cursor", start, now); err != nil {
		t.Fatal(err)
	}
	response := map[string]any{"hook_event_name": "afterAgentResponse", "conversation_id": "native-1", "generation_id": "generation-1", "text": "finished", "model": "label", "model_id": "model-x", "model_params": []any{map[string]any{"id": "effort", "value": "high"}}}
	if err := handleHookEvent(home, "cursor", response, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	regs, _ := store.LoadRegistrations()
	if len(regs) != 1 || regs[0].Harness.Version != "1.7.2" || regs[0].Harness.Mode != "agent" {
		t.Fatalf("registration=%#v", regs)
	}
	requests, _ := store.LoadRequests()
	if len(requests) != 1 || len(requests[0].HookEvidence) != 2 {
		t.Fatalf("requests=%#v", requests)
	}
	final := requests[0].HookEvidence[1]
	if final.Payload["text"] != "finished" || final.Payload["turn_id"] != "generation-1" || final.Payload["model_id"] != "model-x" {
		t.Fatalf("final=%#v", final)
	}
}

func TestHandleHookEventStopForUnregisteredSessionIsNoop(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{"hook_event_name": "Stop", "session_id": "never-registered"}
	if err := handleHookEvent(home, "claude", payload, now); err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(home)
	requests, _ := store.LoadRequests()
	if len(requests) != 0 {
		t.Fatalf("requests=%#v", requests)
	}
}

func TestCursorLifecycleStatusRetainsOnlyDocumentedEnums(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		event string
		field string
		value string
		want  string
	}{
		{"stop", "status", "completed", "completed"},
		{"stop", "status", "incomplete", ""},
		{"stop", "status", "aborted", "aborted"},
		{"stop", "status", "error", "error"},
		// window_close and user_close are sessionEnd reasons, not stop statuses.
		{"stop", "status", "window_close", ""},
		{"sessionEnd", "reason", "aborted", "aborted"},
		{"sessionEnd", "reason", "error", "error"},
		{"sessionEnd", "reason", "window_close", "window_close"},
		{"sessionEnd", "reason", "user_close", "user_close"},
		{"sessionEnd", "reason", "completed", "completed"},
		{"sessionEnd", "reason", "user supplied arbitrary text", ""},
	} {
		evidence, err := filteredHookEvidence(archive.EvidenceKindLifecycleHook, "cursor", tc.event, map[string]any{"hook_event_name": tc.event, tc.field: tc.value}, false, now)
		if err != nil || evidence == nil {
			t.Fatalf("%s: evidence=%#v err=%v", tc.value, evidence, err)
		}
		if got, _ := evidence.Payload["status"].(string); got != tc.want {
			t.Fatalf("%s retained status %q, want %q: %#v", tc.value, got, tc.want, evidence)
		}
	}
}

func TestHandleHookEventIgnoresUnrelatedEvent(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "native-1", "cwd": "/work/widget"}
	if err := handleHookEvent(home, "claude", payload, now); err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 0 {
		t.Fatalf("regs=%#v", regs)
	}
}

func TestHandleHookEventNoopWhenNotConfigured(t *testing.T) {
	t.Parallel()
	home := t.TempDir() // no config.Save call
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": "/work/widget"}
	if err := handleHookEvent(home, "claude", payload, now); err != nil {
		t.Fatal(err)
	}
}

// TestHandleHookEventNoopWhilePaused guards the spec's "Hooks perform no new
// registrations while paused" requirement: pause must stop new tracking at
// the hook, not just at collection/upload time.
func TestHandleHookEventNoopWhilePaused(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if _, err := config.SetPaused(home, true); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	payload := map[string]any{
		"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1",
		"cwd": "/work/widget", "transcript_path": "/tmp/t.jsonl",
	}
	if err := handleHookEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}

	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) != 0 {
		t.Fatalf("a paused hook must register nothing new: regs=%#v", regs)
	}
}

// writeTestTranscript creates a transcript file with the given contents (empty
// for a conversation that has not started yet) and returns its path.
func writeTestTranscript(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A Claude Code worktree lives at <project>/.claude/worktrees/<name>, and a
// session can also start in any subdirectory. Both belong to the configured
// project: they must register under its root, and their later Stop must
// produce a publication request rather than silently finding no registration.
func TestWorktreeAndSubdirectoryStartsRegisterUnderConfiguredProject(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		relative string
	}{
		{"worktree", filepath.Join(".claude", "worktrees", "feature-a")},
		{"subdirectory", filepath.Join("internal", "cli")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home, project := t.TempDir(), t.TempDir()
			setUpTestConfig(t, home, project, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			cwd := filepath.Join(project, tc.relative)
			if err := os.MkdirAll(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
			start := map[string]any{
				"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1",
				"cwd": cwd, "transcript_path": writeTestTranscript(t, "t.jsonl", ""),
			}
			if err := handleHookEvent(home, "claude", start, now); err != nil {
				t.Fatal(err)
			}
			store, _ := state.Open(home)
			regs, err := store.LoadRegistrations()
			if err != nil {
				t.Fatal(err)
			}
			if len(regs) != 1 {
				t.Fatalf("a start in %s did not register: %#v", tc.relative, regs)
			}
			if regs[0].ProjectRoot != project || regs[0].ProjectID != archive.ProjectID(project) {
				t.Fatalf("registered under the working directory instead of the configured project: %#v", regs[0])
			}
			if ds, _ := readCaptureDiagnostics(home); len(ds) != 0 {
				t.Fatalf("an accepted start left a diagnostic: %#v", ds)
			}
			// The consequence this guards: an unregistered start makes every
			// later lifecycle event a no-op, so the session is never published.
			stop := map[string]any{"hook_event_name": "Stop", "session_id": "native-1", "turn_id": "t1"}
			if err := handleHookEvent(home, "claude", stop, now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			requests, err := store.LoadRequests()
			if err != nil || len(requests) != 1 {
				t.Fatalf("stop did not request publication: %#v err=%v", requests, err)
			}
		})
	}
}

// The nearest configured ancestor owns the directory, so a project nested
// inside an included one keeps its own (here: excluded) decision.
func TestNestedExcludedProjectKeepsItsOwnExclusion(t *testing.T) {
	t.Parallel()
	home, parent := t.TempDir(), t.TempDir()
	nested := filepath.Join(parent, "vendor", "secret")
	if err := os.MkdirAll(filepath.Join(nested, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	activated := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := config.Config{
		MachineID: "machine-1", Storage: credentialsTestConfig(),
		Archive: archive.Config{
			SchemaVersion: 1, MachineID: "machine-1", Enabled: true,
			Projects: []archive.ProjectActivation{
				{ProjectID: archive.ProjectID(parent), Root: parent, Included: true, ActivatedAt: activated},
				{ProjectID: archive.ProjectID(nested), Root: nested, Included: false, ActivatedAt: activated},
			},
		},
	}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	start := map[string]any{
		"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1",
		"cwd": filepath.Join(nested, "sub"), "transcript_path": writeTestTranscript(t, "t.jsonl", ""),
	}
	if err := handleHookEvent(home, "claude", start, now); err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 0 {
		t.Fatalf("an excluded nested project was captured through its parent: %#v", regs)
	}
	if ds, _ := readCaptureDiagnostics(home); len(ds) != 0 {
		t.Fatalf("an excluded project left its path on disk: %#v", ds)
	}
}

func TestStartOutsideEveryConfiguredProjectIsSilent(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	// A sibling of the project, not a descendant: string prefix matching alone
	// would wrongly claim it.
	outside := project + "-other"
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	start := map[string]any{
		"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1",
		"cwd": outside, "transcript_path": writeTestTranscript(t, "t.jsonl", ""),
	}
	if err := handleHookEvent(home, "claude", start, now); err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 0 {
		t.Fatalf("a directory outside every project registered: %#v", regs)
	}
	if ds, _ := readCaptureDiagnostics(home); len(ds) != 0 {
		t.Fatalf("a path outside every project was recorded: %#v", ds)
	}
}

// Cursor's sessionStart carries conversation_id, cursor_version,
// workspace_roots, and transcript_path, and no source field at all.
func TestCursorStartUsesTranscriptEmptinessAsFreshStartProof(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		transcript func(t *testing.T) string
		registered bool
	}{
		{"empty transcript is a fresh conversation", func(t *testing.T) string {
			t.Helper()
			return writeTestTranscript(t, "cursor.jsonl", "")
		}, true},
		{"transcript not created yet is a fresh conversation", func(t *testing.T) string {
			t.Helper()
			return filepath.Join(t.TempDir(), "not-created-yet.jsonl")
		}, true},
		{"transcript with bytes is a resume", func(t *testing.T) string {
			t.Helper()
			return writeTestTranscript(t, "cursor.jsonl", "{\"role\":\"user\"}\n")
		}, false},
		// Cursor's desktop app sends a new chat's first hook with
		// transcript_path null (observed on 3.21.13): that is a fresh chat,
		// registered now and given its path by a later hook.
		{"no transcript path is a new desktop chat", func(*testing.T) string { return "" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home, project := t.TempDir(), t.TempDir()
			setUpTestConfig(t, home, project, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
			payload := map[string]any{
				"hook_event_name": "sessionStart", "conversation_id": "conv-1",
				"cursor_version": "1.7.2", "workspace_roots": []any{project},
			}
			if path := tc.transcript(t); path != "" {
				payload["transcript_path"] = path
			}
			if err := handleHookEvent(home, "cursor", payload, now); err != nil {
				t.Fatal(err)
			}
			store, _ := state.Open(home)
			regs, _ := store.LoadRegistrations()
			ds, _ := readCaptureDiagnostics(home)
			if !tc.registered {
				if len(regs) != 0 {
					t.Fatalf("a Cursor resume was registered: %#v", regs)
				}
				if len(ds) != 1 || ds[0].Code != diagnosticUnknownSessionStart {
					t.Fatalf("diagnostics=%#v", ds)
				}
				return
			}
			if len(regs) != 1 || regs[0].Harness.Name != "cursor" || regs[0].Harness.Version != "1.7.2" {
				t.Fatalf("a fresh Cursor conversation was not registered: %#v", regs)
			}
			if regs[0].ProjectRoot != project || len(ds) != 0 {
				t.Fatalf("registration=%#v diagnostics=%#v", regs[0], ds)
			}
		})
	}
}

// The transcript proof is a fallback for Codex and Claude, never an override:
// a documented source still decides when it is present.
func TestCodexAndClaudeKeepTheirSourceRule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		source     string
		contents   string
		registered bool
	}{
		{"resume with an empty transcript stays declined", "resume", "", false},
		{"compact with an empty transcript stays declined", "compact", "", false},
		{"startup with a non-empty transcript still registers", "startup", "{\"type\":\"user\"}\n", true},
		{"missing source falls back to an empty transcript", "", "", true},
		{"missing source with a non-empty transcript is declined", "", "{\"type\":\"user\"}\n", false},
	} {
		for _, harness := range []string{"codex", "claude"} {
			t.Run(harness+": "+tc.name, func(t *testing.T) {
				t.Parallel()
				home, project := t.TempDir(), t.TempDir()
				setUpTestConfig(t, home, project, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
				now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
				payload := map[string]any{
					"hook_event_name": "SessionStart", "session_id": "native-1", "cwd": project,
					"transcript_path": writeTestTranscript(t, "t.jsonl", tc.contents),
				}
				if tc.source != "" {
					payload["source"] = tc.source
				}
				if err := handleHookEvent(home, harness, payload, now); err != nil {
					t.Fatal(err)
				}
				store, _ := state.Open(home)
				regs, _ := store.LoadRegistrations()
				if tc.registered != (len(regs) == 1) {
					t.Fatalf("registered=%t want %t: %#v", len(regs) == 1, tc.registered, regs)
				}
			})
		}
	}
}

func TestSetupInProgressLeavesNoDiagnosticForUnconfiguredPaths(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err := os.WriteFile(journalPath(home), []byte(`{"changes":[],"plist":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Outside every configured project, and a non-start event inside one.
	outside := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "n1", "cwd": t.TempDir()}
	if err := handleHookEvent(home, "claude", outside, now); err != nil {
		t.Fatal(err)
	}
	if err := handleHookEvent(home, "claude", map[string]any{"hook_event_name": "Stop", "session_id": "n1", "cwd": project}, now); err != nil {
		t.Fatal(err)
	}
	if ds, _ := readCaptureDiagnostics(home); len(ds) != 0 {
		t.Fatalf("diagnostics=%#v", ds)
	}
}

// A continuation of a worktree session reports the worktree directory again.
// It must match the registration made under the configured root, keep the
// original start time, and leave no diagnostic. The project is configured
// under a symlinked spelling while the hook reports the resolved one, so the
// match has to go through resolved paths on both sides.
func TestWorktreeContinuationsMatchTheConfiguredRegistration(t *testing.T) {
	t.Parallel()
	home, resolved := t.TempDir(), t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(resolved, alias); err != nil {
		t.Fatal(err)
	}
	setUpTestConfig(t, home, alias, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	worktree := filepath.Join(resolved, ".claude", "worktrees", "feature-a")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	transcript := filepath.Join(t.TempDir(), "not-created-yet.jsonl")
	start := map[string]any{
		"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1",
		"cwd": worktree, "transcript_path": transcript,
	}
	if err := handleHookEvent(home, "claude", start, started); err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 1 || regs[0].ProjectRoot != alias || regs[0].ProjectID != archive.ProjectID(alias) {
		t.Fatalf("a worktree start under a symlinked project did not register under the configured spelling: %#v", regs)
	}
	for _, source := range []string{"resume", "compact"} {
		continuation := map[string]any{
			"hook_event_name": "SessionStart", "source": source, "session_id": "native-1",
			"cwd": worktree, "transcript_path": transcript,
		}
		if err := handleHookEvent(home, "claude", continuation, started.Add(time.Hour)); err != nil {
			t.Fatalf("%s from the worktree conflicted with its own registration: %v", source, err)
		}
	}
	regs, _ = store.LoadRegistrations()
	if len(regs) != 1 || !regs[0].SessionStartedAt.Equal(started) || regs[0].ProjectRoot != alias {
		t.Fatalf("continuations changed the registration: %#v", regs)
	}
	if ds, _ := readCaptureDiagnostics(home); len(ds) != 0 {
		t.Fatalf("continuations left a diagnostic: %#v", ds)
	}
}

// A relative transcript path would be resolved against the hook process's
// working directory, where it never exists, so "not found" would pass as
// proof. Only an absolute path can carry the proof.
func TestRelativeTranscriptPathProvesNothing(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"transcript.jsonl", "~/transcript.jsonl", filepath.Join("sessions", "transcript.jsonl")} {
		if emptyTranscriptProvesFreshStart(map[string]any{"transcript_path": path}) {
			t.Fatalf("%q was accepted as proof of a fresh start", path)
		}
	}
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{
		"hook_event_name": "sessionStart", "conversation_id": "conv-1",
		"workspace_roots": []any{project}, "transcript_path": "not-created-yet.jsonl",
	}
	if err := handleHookEvent(home, "cursor", payload, now); err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(home)
	if regs, _ := store.LoadRegistrations(); len(regs) != 0 {
		t.Fatalf("a relative transcript path registered a session: %#v", regs)
	}
	if ds, _ := readCaptureDiagnostics(home); len(ds) != 1 || ds[0].Code != diagnosticUnknownSessionStart {
		t.Fatalf("diagnostics=%#v", ds)
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestResumeCannotReplaceIdentityOrEraseTranscript(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Now().UTC()
	setUpTestConfig(t, home, "/work/widget", at.Add(-time.Hour))
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "s", "cwd": "/work/widget", "transcript_path": "/synthetic/transcript.jsonl"}
	if err := handleHookEvent(home, "claude", payload, at); err != nil {
		t.Fatal(err)
	}
	delete(payload, "transcript_path")
	payload["source"] = "resume"
	if err := handleHookEvent(home, "claude", payload, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := handleHookEvent(home, "codex", payload, at.Add(time.Minute)); err == nil {
		t.Fatal("cross-harness identity accepted")
	}
	if err := handleHookEvent(home, "codex", map[string]any{"hook_event_name": "Stop", "session_id": "s"}, at.Add(time.Minute)); err == nil {
		t.Fatal("cross-harness stop accepted")
	}
	store, _ := state.Open(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 1 || regs[0].TranscriptPath != "/synthetic/transcript.jsonl" || !regs[0].SessionStartedAt.Equal(at) {
		t.Fatalf("registration %+v", regs)
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestCompactFromSubdirectoryKeepsProjectIdentity(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Now().UTC()
	cfg := config.Config{
		MachineID: "machine-1",
		Storage:   credentialsTestConfig(),
		Archive: archive.Config{
			SchemaVersion: 1, MachineID: "machine-1", Enabled: true,
			Projects: []archive.ProjectActivation{
				{ProjectID: archive.ProjectID("/work/widget"), Root: "/work/widget", Included: true, ActivatedAt: at.Add(-time.Hour)},
				{ProjectID: archive.ProjectID("/work/other"), Root: "/work/other", Included: true, ActivatedAt: at.Add(-time.Hour)},
			},
		},
	}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	start := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "s", "cwd": "/work/widget", "transcript_path": "/synthetic/t1.jsonl"}
	if err := handleHookEvent(home, "claude", start, at); err != nil {
		t.Fatal(err)
	}
	// The agent `cd`'d into a subdirectory; Claude Code's hook cwd follows it.
	compact := map[string]any{"hook_event_name": "SessionStart", "source": "compact", "session_id": "s", "cwd": "/work/widget/internal/cli", "transcript_path": "/synthetic/t2.jsonl", "model": "model-x"}
	if err := handleHookEvent(home, "claude", compact, at.Add(time.Minute)); err != nil {
		t.Fatalf("compact from a subdirectory of the registered project was rejected: %v", err)
	}
	store, _ := state.Open(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 1 || regs[0].ProjectRoot != "/work/widget" || regs[0].TranscriptPath != "/synthetic/t2.jsonl" || !regs[0].SessionStartedAt.Equal(at) {
		t.Fatalf("registration %+v", regs)
	}
	// Both the fresh start and the compact continuation are lifecycle
	// evidence for the same session; neither asks for an upload.
	requests, err := store.LoadRequests()
	if err != nil || len(requests) != 1 || len(requests[0].HookEvidence) != 2 || !requests[0].Deferred {
		t.Fatalf("compact lifecycle evidence was not recorded: %+v err=%v", requests, err)
	}
	if last := requests[0].HookEvidence[1]; last.Provenance != "hook:claude:sessionstart" || last.Payload["model"] != "model-x" {
		t.Fatalf("compact lifecycle evidence was not recorded: %+v", last)
	}
	// A continuation reported from a different configured project is still a conflict.
	compact["cwd"] = "/work/other/sub"
	if err := handleHookEvent(home, "claude", compact, at.Add(2*time.Minute)); err == nil {
		t.Fatal("cross-project identity accepted")
	}
	regs, _ = store.LoadRegistrations()
	if len(regs) != 1 || regs[0].ProjectRoot != "/work/widget" || regs[0].TranscriptPath != "/synthetic/t2.jsonl" {
		t.Fatalf("registration %+v", regs)
	}
}
