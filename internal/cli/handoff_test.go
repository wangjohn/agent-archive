package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// handoffTranscript is a Codex session with a prompt, a tool call whose
// output is a list of blocks, a credential, and a closing reply.
const handoffTranscript = `{"type":"session_meta","timestamp":"2026-01-02T00:00:00Z","payload":{"id":"native-1","cwd":"PROJECT"}}
{"type":"turn_context","timestamp":"2026-01-02T00:00:01Z","payload":{"cwd":"PROJECT","model":"gpt-test"}}
{"type":"response_item","timestamp":"2026-01-02T00:00:02Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Fix the flaky widget test. Use api_key=hunter2secret for staging."}]}}
{"type":"response_item","timestamp":"2026-01-02T00:00:03Z","payload":{"type":"function_call","name":"exec_command","call_id":"call_1","arguments":"{\"cmd\":\"go test ./widget\"}"}}
{"type":"response_item","timestamp":"2026-01-02T00:00:04Z","payload":{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"FAIL TestWidget\nwidget_test.go:9: flaky"}]}}
{"type":"response_item","timestamp":"2026-01-02T00:00:05Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"The widget test races on the clock; next I will inject a fake clock."}]}}
`

type handoffFixture struct {
	env     Env
	home    string
	project string
	mem     *storage.MemoryStore
	id      string
}

// newHandoffFixture registers one Codex session through the hook path. It
// is not synced unless sync is true, so a local handoff must not depend on
// an upload.
func newHandoffFixture(t *testing.T, sync bool) handoffFixture {
	t.Helper()
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	transcript := filepath.Join(project, "codex.jsonl")
	if err := os.WriteFile(transcript, []byte(strings.ReplaceAll(handoffTranscript, "PROJECT", project)), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC)
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": project, "transcript_path": transcript}
	// Registration needs an empty transcript at start; write the content after.
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := handleHookEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte(strings.ReplaceAll(handoffTranscript, "PROJECT", project)), 0o600); err != nil {
		t.Fatal(err)
	}
	mem := storage.NewMemoryStore()
	env := testEnv(t, home, now)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return mem, nil }
	env.WorkingDir = func() (string, error) { return project, nil }
	f := handoffFixture{env: env, home: home, project: project, mem: mem}
	if sync {
		var out, errOut bytes.Buffer
		if code := runSyncCommand(nil, &out, &errOut, env); code != 0 {
			t.Fatalf("sync code=%d stderr=%s", code, errOut.String())
		}
	}
	f.id = onlyRegistrationID(t, home)
	return f
}

func onlyRegistrationID(t *testing.T, home string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, "registrations"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("registrations = %v, %v", entries, err)
	}
	return strings.TrimSuffix(entries[0].Name(), ".json")
}

func runHandoff(t *testing.T, env Env, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(append([]string{"handoff"}, args...), nil, &out, &errOut, env)
	return out.String(), errOut.String(), code
}

func TestHandoffRejectsBadArguments(t *testing.T) {
	env := testEnv(t, t.TempDir(), time.Now())
	for _, args := range [][]string{
		{},
		{"abc", "--latest"},
		{"--latest", "--file", "x.jsonl", "--harness", "codex"},
		{"--file", "x.jsonl"},
		{"--latest", "--source", "cloud"},
		{"--latest", "--format", "html"},
		{"--latest", "--harness", "gemini"},
		{"abc", "--project", "/tmp"},
		{"abc", "--force"},
		{"--file", "x.jsonl", "--harness", "codex", "--source", "archive"},
		{"abc", "extra"},
	} {
		if _, errOut, code := runHandoff(t, env, args...); code != 2 || errOut == "" {
			t.Errorf("%v: code=%d stderr=%q", args, code, errOut)
		}
	}
}

// claude "$(agent-archive handoff --latest)" must not start a session whose
// prompt is the not-set-up message: it goes to stderr, with exit 1.
func TestHandoffReportsNotSetUp(t *testing.T) {
	out, errOut, code := runHandoff(t, testEnv(t, t.TempDir(), time.Now()), "--latest")
	if code != 1 || out != "" || !strings.Contains(errOut, "Not set up") {
		t.Fatalf("code=%d out=%q stderr=%q", code, out, errOut)
	}
}

// A session on this machine is handed off from its transcript as it is now,
// with no sync, filtered exactly as the archive would be.
func TestHandoffReadsLocalSessionWithoutSync(t *testing.T) {
	f := newHandoffFixture(t, false)
	for _, args := range [][]string{{f.id}, {"--latest"}} {
		out, errOut, code := runHandoff(t, f.env, args...)
		if code != 0 {
			t.Fatalf("%v: code=%d stderr=%s", args, code, errOut)
		}
		for _, want := range []string{"source: local", "Fix the flaky widget test.", "`exec_command` go test ./widget", "widget_test.go:9: flaky", "## Where it left off", "inject a fake clock"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%v: output missing %q:\n%s", args, want, out)
			}
		}
		if strings.Contains(out, "hunter2secret") {
			t.Fatalf("%v: credential leaked:\n%s", args, out)
		}
	}
	if keys, _ := f.mem.List(t.Context(), ""); len(keys) != 0 {
		t.Fatalf("a local handoff uploaded %d objects", len(keys))
	}
}

func TestHandoffLatestNamesItsChoiceOnStderr(t *testing.T) {
	f := newHandoffFixture(t, false)
	sub := filepath.Join(f.project, "widget")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	f.env.WorkingDir = func() (string, error) { return sub, nil }
	_, errOut, code := runHandoff(t, f.env, "--latest")
	if code != 0 || !strings.Contains(errOut, "handoff: using codex session "+f.id) || !strings.Contains(errOut, "(this machine)") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

// The same session handed off from the local transcript and from the archive
// renders the same conversation.
func TestHandoffLocalAndArchiveRenderTheSameConversation(t *testing.T) {
	f := newHandoffFixture(t, true)
	localOut, errOut, code := runHandoff(t, f.env, f.id, "--source", "local")
	if code != 0 {
		t.Fatalf("local: code=%d stderr=%s", code, errOut)
	}
	archiveOut, errOut, code := runHandoff(t, f.env, f.id, "--source", "archive")
	if code != 0 {
		t.Fatalf("archive: code=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(archiveOut, "source: archive") {
		t.Fatalf("archive output:\n%s", archiveOut)
	}
	conversation := func(s string) string { return s[strings.Index(s, "## Where it left off"):] }
	if conversation(localOut) != conversation(archiveOut) {
		t.Fatalf("local and archive differ:\n--- local\n%s\n--- archive\n%s", localOut, archiveOut)
	}
	// --latest finds the archived copy by project ID when told to skip local.
	_, errOut, code = runHandoff(t, f.env, "--latest", "--source", "archive")
	if code != 0 || !strings.Contains(errOut, "captured") {
		t.Fatalf("latest archive: code=%d stderr=%s", code, errOut)
	}
}

// With no session for the project, --latest lists recent archived sessions
// from metadata only.
func TestHandoffLatestWithoutMatchListsRecentSessions(t *testing.T) {
	f := newHandoffFixture(t, true)
	f.env.WorkingDir = func() (string, error) { return t.TempDir(), nil }
	out, errOut, code := runHandoff(t, f.env, "--latest")
	if code != 1 || out != "" {
		t.Fatalf("code=%d out=%q", code, out)
	}
	for _, want := range []string{"no session for", "Recent archived sessions:", "codex", "this machine", "agent-archive handoff " + f.id} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("stderr missing %q:\n%s", want, errOut)
		}
	}
	if strings.Contains(errOut, "flaky") {
		t.Fatalf("fallback list printed transcript content:\n%s", errOut)
	}
}

// Trimming saves the untrimmed version, names it in the footer, and prunes
// saved handoffs older than seven days.
func TestHandoffSavesFullVersionWhenTrimmed(t *testing.T) {
	f := newHandoffFixture(t, false)
	stale := filepath.Join(f.home, handoffDir, "old.md")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := f.env.now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	// Thirty more tool calls give the budget something outside the protected
	// tail to trim.
	var extra strings.Builder
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&extra, `{"type":"response_item","timestamp":"2026-01-02T00:01:%02dZ","payload":{"type":"function_call","name":"exec_command","call_id":"call_x%d","arguments":"{\"cmd\":\"go test -run T%d\"}"}}`+"\n", i, i, i)
		fmt.Fprintf(&extra, `{"type":"response_item","timestamp":"2026-01-02T00:01:%02dZ","payload":{"type":"function_call_output","call_id":"call_x%d","output":"%s"}}`+"\n", i, i, strings.Repeat("ok ", 200))
	}
	transcript := filepath.Join(f.project, "codex.jsonl")
	content := strings.ReplaceAll(handoffTranscript, "PROJECT", f.project)
	if err := os.WriteFile(transcript, []byte(content+extra.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	full, _, code := runHandoff(t, f.env, f.id)
	if code != 0 || strings.Contains(full, "Full record") {
		t.Fatalf("untrimmed: code=%d\n%s", code, full)
	}
	saved := filepath.Join(f.home, handoffDir, f.id+".md")
	if _, err := os.Stat(saved); !os.IsNotExist(err) {
		t.Fatalf("a full version was saved without trimming: %v", err)
	}

	trimmed, errOut, code := runHandoff(t, f.env, f.id, "--max-bytes", "8000")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(trimmed, "Full record: "+saved) {
		t.Fatalf("footer does not name %s:\n%s", saved, trimmed)
	}
	info, err := os.Stat(saved)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved full version: %v %v", info, err)
	}
	body, _ := os.ReadFile(saved)
	if string(body) != full {
		t.Fatalf("saved version is not the untrimmed rendering:\n%s", body)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale handoff not pruned: %v", err)
	}
}

func TestHandoffOutputFileIsPrivateAndNotOverwritten(t *testing.T) {
	f := newHandoffFixture(t, false)
	path := filepath.Join(t.TempDir(), "handoff.md")
	if _, errOut, code := runHandoff(t, f.env, f.id, "--output", path); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("output: %v %v", info, err)
	}
	if _, errOut, code := runHandoff(t, f.env, f.id, "--output", path); code != 1 || !strings.Contains(errOut, "--force") {
		t.Fatalf("overwrite: code=%d stderr=%s", code, errOut)
	}
	if _, errOut, code := runHandoff(t, f.env, f.id, "--output", path, "--force"); code != 0 {
		t.Fatalf("force: code=%d stderr=%s", code, errOut)
	}
}

// --file renders a transcript the archive never registered, with no setup,
// through the same privacy filter.
func TestHandoffFileNeedsNoSetupAndIsFiltered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	content := `{"type":"user","uuid":"u1","sessionId":"native-claude","timestamp":"2026-01-02T00:00:00Z","cwd":"/work/repo","message":{"role":"user","content":"<system-reminder>private memory</system-reminder>Ship it with password=opensesame"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"native-claude","timestamp":"2026-01-02T00:00:01Z","message":{"id":"m1","role":"assistant","model":"claude-test","content":[{"type":"text","text":"Shipping."}]}}
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := runHandoff(t, testEnv(t, t.TempDir(), time.Now()), "--file", path, "--harness", "claude", "--format", "json")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	for _, leaked := range []string{"opensesame", "private memory"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("%q leaked:\n%s", leaked, out)
		}
	}
	if !strings.Contains(out, `"source": "file"`) || !strings.Contains(out, `"native_session_id": "native-claude"`) {
		t.Fatalf("json output:\n%s", out)
	}
}

func TestUninstallOwnsHandoffDirectory(t *testing.T) {
	for _, entry := range localStateEntries {
		if entry == handoffDir {
			return
		}
	}
	t.Fatalf("localStateEntries does not include %q", handoffDir)
}

// Run inside an agent, --latest skips the session running the command: it is
// always the newest, and handing an agent its own conversation is useless.
func TestHandoffLatestSkipsTheCallingAgentSession(t *testing.T) {
	f := newHandoffFixture(t, true)
	f.env.LookupEnv = func(key string) (string, bool) {
		if key == "CLAUDE_CODE_SESSION_ID" {
			return "native-1", true
		}
		return "", false
	}
	out, errOut, code := runHandoff(t, f.env, "--latest")
	if code != 1 || out != "" || !strings.Contains(errOut, "no session for") {
		t.Fatalf("the calling session was not skipped: code=%d stderr=%s", code, errOut)
	}
	// An explicit ID is still honored.
	if _, errOut, code := runHandoff(t, f.env, f.id); code != 0 {
		t.Fatalf("explicit ID: code=%d stderr=%s", code, errOut)
	}
}

// A newer session that has only just started (empty transcript, no prompt)
// is passed over for the older one with content, rather than failing.
func TestHandoffLatestPassesOverSessionsWithoutPrompts(t *testing.T) {
	f := newHandoffFixture(t, false)
	fresh := filepath.Join(f.project, "fresh.jsonl")
	if err := os.WriteFile(fresh, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-2", "cwd": f.project, "transcript_path": fresh}
	if err := handleHookEvent(f.home, "codex", payload, f.env.now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(fresh, later, later); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := runHandoff(t, f.env, "--latest")
	if code != 0 || !strings.Contains(errOut, "using codex session "+f.id) || !strings.Contains(out, "Fix the flaky widget test.") {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
}

func TestHandoffRejectsUnsafeSessionIDs(t *testing.T) {
	f := newHandoffFixture(t, false)
	for _, id := range []string{"../registrations/x", "a/b", "..", "."} {
		if _, errOut, code := runHandoff(t, f.env, id); code != 2 || !strings.Contains(errOut, "not an archive session ID") {
			t.Errorf("%q: code=%d stderr=%s", id, code, errOut)
		}
	}
}

// When the local transcript exists but cannot be used, --source auto falls
// back to the archive's published copy.
func TestHandoffFallsBackToArchiveWhenLocalTranscriptIsUnusable(t *testing.T) {
	f := newHandoffFixture(t, true)
	if err := os.WriteFile(filepath.Join(f.project, "codex.jsonl"), []byte("not json at all\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := runHandoff(t, f.env, f.id)
	if code != 0 || !strings.Contains(out, "source: archive") || !strings.Contains(out, "Fix the flaky widget test.") {
		t.Fatalf("code=%d stderr=%s\n%s", code, errOut, out)
	}
	if _, errOut, code := runHandoff(t, f.env, f.id, "--source", "local"); code != 1 || errOut == "" {
		t.Fatalf("--source local must report the local failure: code=%d", code)
	}
}

// Running from a parent directory does not pick up the projects beneath it.
func TestHandoffLatestFromAParentDirectoryDoesNotMatchChildProjects(t *testing.T) {
	f := newHandoffFixture(t, true)
	_, errOut, code := runHandoff(t, f.env, "--latest", "--project", filepath.Dir(f.project))
	if code != 1 || !strings.Contains(errOut, "no session for") {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
}

// Without setup, --file never creates the data directory, even when the
// output is trimmed.
func TestHandoffFileWithoutSetupDoesNotCreateTheDataDirectory(t *testing.T) {
	var transcript strings.Builder
	transcript.WriteString(`{"type":"user","uuid":"u1","timestamp":"2026-01-02T00:00:00Z","message":{"role":"user","content":"go"}}` + "\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&transcript, `{"type":"assistant","uuid":"a%d","timestamp":"2026-01-02T00:00:01Z","message":{"id":"m%d","role":"assistant","content":[{"type":"tool_use","id":"t%d","name":"Bash","input":{"command":"echo %d"}}]}}`+"\n", i, i, i, i)
		fmt.Fprintf(&transcript, `{"type":"user","uuid":"r%d","timestamp":"2026-01-02T00:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t%d","content":"%s"}]}}`+"\n", i, i, strings.Repeat("x", 500))
	}
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(transcript.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "never-set-up")
	out, errOut, code := runHandoff(t, testEnv(t, home, time.Now()), "--file", path, "--harness", "claude", "--max-bytes", "6000")
	if code != 0 || !strings.Contains(out, "Omitted to fit") || strings.Contains(out, "Full record") {
		t.Fatalf("code=%d stderr=%s\n%s", code, errOut, out)
	}
	if !strings.Contains(errOut, "not saved") {
		t.Fatalf("stderr does not explain the missing full version: %s", errOut)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("data directory created: %v", err)
	}
}
