package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// cursorDesktopPayload is a Cursor desktop hook payload in the shape observed
// on 3.21.13: every event carries conversation_id and session_id (the same
// value), cursor_version, workspace_roots, composer_mode, model, model_id, and
// model_params. transcript is the transcript_path value; nil sends JSON null.
func cursorDesktopPayload(event, conversation, project string, transcript any) map[string]any {
	return map[string]any{
		"hook_event_name": event, "conversation_id": conversation, "session_id": conversation,
		"cursor_version": "3.21.13", "workspace_roots": []any{project}, "composer_mode": "agent",
		"model": "synthetic-model", "model_id": "synthetic-model-id",
		"model_params":    []any{map[string]any{"id": "effort", "value": "high"}},
		"transcript_path": transcript,
	}
}

// cursorTranscriptLocation mirrors Cursor's own layout,
// ~/.cursor/projects/<workspace>/agent-transcripts/<id>/<id>.jsonl, under a
// temporary directory.
func cursorTranscriptLocation(t *testing.T, conversation string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "projects", "work-widget", "agent-transcripts", conversation)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, conversation+".jsonl")
}

func onlyCursorRegistration(t *testing.T, home string) archive.SessionRegistration {
	t.Helper()
	store, err := collector.NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("registrations = %#v err=%v", regs, err)
	}
	return regs[0]
}

// A new desktop chat fires no sessionStart. Its first beforeSubmitPrompt has
// transcript_path null; afterAgentResponse and stop then name the transcript.
// The chat must register at the first prompt, wait without error until the
// path arrives, and then publish.
func TestCursorDesktopChatRegistersAtFirstPromptAndPublishes(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	first := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	conversation := "5f3c2a10-0000-4000-8000-00000000c3c3"
	transcript := cursorTranscriptLocation(t, conversation)

	if err := handleHookEvent(home, "cursor", cursorDesktopPayload("beforeSubmitPrompt", conversation, project, nil), first); err != nil {
		t.Fatal(err)
	}
	reg := onlyCursorRegistration(t, home)
	if reg.NativeSessionID != conversation || reg.TranscriptPath != "" || !reg.SessionStartedAt.Equal(first) || reg.ProjectRoot != project {
		t.Fatalf("registration = %#v", reg)
	}
	if reg.Harness.Version != "3.21.13" || reg.Harness.Mode != "agent" {
		t.Fatalf("harness = %#v", reg.Harness)
	}

	// A collection pass while the path is still unknown is waiting, not failing.
	mem := storage.NewMemoryStore()
	env := testEnv(t, home, first.Add(time.Second))
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return mem, nil }
	var stdout, stderr bytes.Buffer
	if code := runSyncCommand(nil, &stdout, &stderr, env); code != 0 || strings.Contains(stdout.String(), "1 failed") {
		t.Fatalf("waiting registration failed the pass: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	store, _ := collector.NewLocalStore(home)
	if status, _ := store.LoadStatus(); status.LastError != "" {
		t.Fatalf("waiting registration recorded an error: %q", status.LastError)
	}

	// Cursor writes the transcript, then names it on the response and stop.
	lines := []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"Summarize the widget package."}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"It parses widget files."}]}}`,
	}
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := handleHookEvent(home, "cursor", cursorDesktopPayload("afterAgentResponse", conversation, project, transcript), first.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if reg = onlyCursorRegistration(t, home); reg.TranscriptPath != transcript {
		t.Fatalf("transcript path was not recorded: %#v", reg)
	}
	stop := cursorDesktopPayload("stop", conversation, project, transcript)
	stop["status"] = "completed"
	if err := handleHookEvent(home, "cursor", stop, first.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	env.Now = func() time.Time { return first.Add(time.Minute) }
	if code := runSyncCommand(nil, &stdout, &stderr, env); code != 0 || !strings.Contains(stdout.String(), "1 published") {
		t.Fatalf("chat was not published: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	key, err := archive.MetadataObjectKey("cursor", reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := mem.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Counts.Turns == nil || *metadata.Counts.Turns != 1 || metadata.Harness.Version != "3.21.13" {
		t.Fatalf("published metadata = %#v", metadata)
	}
}

// A resumed chat's first beforeSubmitPrompt already names its non-empty
// transcript: that proves nothing about when the chat began, so it is declined
// with the existing diagnostic.
func TestCursorResumedChatIsDeclinedAtFirstPrompt(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	conversation := "5f3c2a10-0000-4000-8000-00000000d0d0"
	transcript := cursorTranscriptLocation(t, conversation)
	if err := os.WriteFile(transcript, []byte(`{"role":"user","message":{"content":[{"type":"text","text":"earlier"}]}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	for _, event := range []string{"beforeSubmitPrompt", "afterAgentResponse", "stop", "sessionEnd"} {
		if err := handleHookEvent(home, "cursor", cursorDesktopPayload(event, conversation, project, transcript), at); err != nil {
			t.Fatal(err)
		}
	}
	store, _ := collector.NewLocalStore(home)
	if regs, _ := store.LoadRegistrations(); len(regs) != 0 {
		t.Fatalf("a resumed chat was registered: %#v", regs)
	}
	ds, _ := readCaptureDiagnostics(home)
	if len(ds) != 1 || ds[0].Code != diagnosticUnknownSessionStart || ds[0].Harness != "cursor" || ds[0].ProjectRoot != project {
		t.Fatalf("diagnostics = %#v", ds)
	}

	// Outside every configured project nothing is recorded at all.
	outside := t.TempDir()
	other := cursorDesktopPayload("beforeSubmitPrompt", "other-conversation", outside, transcript)
	if err := handleHookEvent(home, "cursor", other, at); err != nil {
		t.Fatal(err)
	}
	if after, _ := readCaptureDiagnostics(home); len(after) != 1 {
		t.Fatalf("a path outside every project was recorded: %#v", after)
	}
}

// Only a path that is absolute and named <conversation_id>.jsonl is taken as
// the chat's transcript, and once set it is never replaced.
func TestCursorTranscriptPathMustMatchTheConversation(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	conversation := "5f3c2a10-0000-4000-8000-00000000e0e0"
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	if err := handleHookEvent(home, "cursor", cursorDesktopPayload("beforeSubmitPrompt", conversation, project, nil), at); err != nil {
		t.Fatal(err)
	}
	wrongName := filepath.Join(filepath.Dir(cursorTranscriptLocation(t, conversation)), "another-conversation.jsonl")
	for _, path := range []string{wrongName, "relative/" + conversation + ".jsonl", filepath.Join(t.TempDir(), conversation+".txt")} {
		if err := handleHookEvent(home, "cursor", cursorDesktopPayload("afterAgentResponse", conversation, project, path), at.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if reg := onlyCursorRegistration(t, home); reg.TranscriptPath != "" {
			t.Fatalf("path %q was adopted: %#v", path, reg)
		}
	}
	right := cursorTranscriptLocation(t, conversation)
	if err := handleHookEvent(home, "cursor", cursorDesktopPayload("stop", conversation, project, right), at.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if reg := onlyCursorRegistration(t, home); reg.TranscriptPath != right {
		t.Fatalf("matching path not adopted: %#v", reg)
	}
	elsewhere := cursorTranscriptLocation(t, conversation)
	for _, event := range []string{"beforeSubmitPrompt", "afterAgentResponse", "stop", "sessionStart"} {
		if err := handleHookEvent(home, "cursor", cursorDesktopPayload(event, conversation, project, elsewhere), at.Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
		if reg := onlyCursorRegistration(t, home); reg.TranscriptPath != right {
			t.Fatalf("%s replaced the transcript path: %#v", event, reg)
		}
	}
}

// Codex and Claude Code keep their sessionStart rules: a first prompt from a
// session that never started is still ignored, and a start without a source
// or a transcript still proves nothing.
func TestFirstPromptRegistrationIsCursorOnly(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	for harness, event := range map[string]string{"codex": "UserPromptSubmit", "claude": "UserPromptSubmit"} {
		payload := map[string]any{"hook_event_name": event, "session_id": harness + "-session", "cwd": project}
		if err := handleHookEvent(home, harness, payload, at); err != nil {
			t.Fatal(err)
		}
		start := map[string]any{"hook_event_name": "SessionStart", "session_id": harness + "-start", "cwd": project}
		if err := handleHookEvent(home, harness, start, at); err != nil {
			t.Fatal(err)
		}
	}
	store, _ := collector.NewLocalStore(home)
	if regs, _ := store.LoadRegistrations(); len(regs) != 0 {
		t.Fatalf("a non-Cursor session registered without its documented start proof: %#v", regs)
	}
}

// Hooks for one new chat can overlap: a first prompt, and a fast response
// and stop. hooks.lock serializes them, so whichever lands first registers
// and the rest continue it: one registration, one archive ID, the first
// hook's time as the start.
func TestCursorOverlappingHooksRegisterOnce(t *testing.T) {
	for i := 0; i < 10; i++ {
		home, project := t.TempDir(), t.TempDir()
		setUpTestConfig(t, home, project, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
		at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
		conversation := "5f3c2a10-0000-4000-8000-00000000ace0"
		transcript := cursorTranscriptLocation(t, conversation)
		if err := os.WriteFile(transcript, []byte(`{"role":"user","message":{"content":[{"type":"text","text":"hi"}]}}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make(chan error, 3)
		for _, event := range []string{"beforeSubmitPrompt", "afterAgentResponse", "stop"} {
			wg.Add(1)
			go func(event string) {
				defer wg.Done()
				var path any = transcript
				if event == "beforeSubmitPrompt" {
					path = nil
				}
				errs <- handleHookEvent(home, "cursor", cursorDesktopPayload(event, conversation, project, path), at)
			}(event)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		store, _ := collector.NewLocalStore(home)
		regs, err := store.LoadRegistrations()
		if err != nil || len(regs) != 1 {
			t.Fatalf("iteration %d: registrations = %#v err=%v", i, regs, err)
		}
		id, found, err := store.ArchiveSessionID(conversation)
		if err != nil || !found || id != regs[0].ArchiveSessionID || !regs[0].SessionStartedAt.Equal(at) {
			t.Fatalf("iteration %d: index=%q found=%v err=%v registration=%#v", i, id, found, err, regs[0])
		}
	}
}

// status reports a chat that is still waiting for its transcript as a hook
// observed and capture pending, with no error.
func TestCursorWaitingChatShowsInStatus(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Harnesses = []string{"cursor"}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	if err := handleHookEvent(home, "cursor", cursorDesktopPayload("beforeSubmitPrompt", "5f3c2a10-0000-4000-8000-00000000aaaa", project, nil), at); err != nil {
		t.Fatal(err)
	}
	env := testEnv(t, home, at.Add(time.Second))
	var stdout, stderr bytes.Buffer
	if code := runSyncCommand(nil, &stdout, &stderr, env); code != 0 {
		t.Fatalf("sync: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if code := runStatusCommand(nil, &stdout, &stderr, env); code != 0 {
		t.Fatalf("status: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if out := stdout.String(); strings.Contains(out, "Last error") || !strings.Contains(out, "hook observed; waiting for capture") || !strings.Contains(out, "Pending:       1 session(s)") {
		t.Fatalf("status = %s", out)
	}
}

// The user comes back to a chat later. Its next beforeSubmitPrompt names the
// now non-empty transcript; that is a continuation of the registered chat,
// which keeps its start time and takes the path, and no diagnostic is
// recorded. A sessionStart on the same chat is a continuation too.
func TestCursorLaterPromptContinuesTheRegisteredChat(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	first := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	conversation := "5f3c2a10-0000-4000-8000-00000000bbbb"
	transcript := cursorTranscriptLocation(t, conversation)
	if err := handleHookEvent(home, "cursor", cursorDesktopPayload("beforeSubmitPrompt", conversation, project, nil), first); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte(`{"role":"user","message":{"content":[{"type":"text","text":"hi"}]}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := first.Add(time.Hour)
	if err := handleHookEvent(home, "cursor", cursorDesktopPayload("beforeSubmitPrompt", conversation, project, transcript), later); err != nil {
		t.Fatal(err)
	}
	reg := onlyCursorRegistration(t, home)
	if !reg.SessionStartedAt.Equal(first) || reg.TranscriptPath != transcript {
		t.Fatalf("registration = %#v", reg)
	}
	if ds, _ := readCaptureDiagnostics(home); len(ds) != 0 {
		t.Fatalf("a continuation recorded diagnostics: %#v", ds)
	}
	other := cursorTranscriptLocation(t, conversation)
	if err := handleHookEvent(home, "cursor", cursorDesktopPayload("sessionStart", conversation, project, other), later.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if reg = onlyCursorRegistration(t, home); !reg.SessionStartedAt.Equal(first) || reg.TranscriptPath != transcript {
		t.Fatalf("registration after sessionStart = %#v", reg)
	}
}

// workspace_roots resolves to the nearest configured ancestor. A chat in an
// excluded project nested inside an included one records nothing at any
// event: no registration, no archive ID, no request, no diagnostic. A chat
// elsewhere under the included parent registers under the parent's root.
func TestCursorFirstPromptHonorsNearestConfiguredProject(t *testing.T) {
	home, parent := t.TempDir(), t.TempDir()
	nested := filepath.Join(parent, "nested")
	if err := os.MkdirAll(filepath.Join(nested, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	activated := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	cfg := config.Config{
		MachineID: "machine-1", Storage: credentialsTestConfig(),
		Archive: archive.Config{SchemaVersion: 1, MachineID: "machine-1", Enabled: true,
			Projects: []archive.ProjectActivation{
				{ProjectID: archive.ProjectID(parent), Root: parent, Included: true, ActivatedAt: activated},
				{ProjectID: archive.ProjectID(nested), Root: nested, Included: false, ActivatedAt: activated},
			}},
	}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	excluded := "5f3c2a10-0000-4000-8000-00000000cccc"
	transcript := cursorTranscriptLocation(t, excluded)
	for _, event := range []string{"beforeSubmitPrompt", "afterAgentResponse", "stop", "sessionEnd"} {
		var path any = transcript
		if event == "beforeSubmitPrompt" {
			path = nil
		}
		if err := handleHookEvent(home, "cursor", cursorDesktopPayload(event, excluded, filepath.Join(nested, "sub"), path), at); err != nil {
			t.Fatal(err)
		}
	}
	store, _ := collector.NewLocalStore(home)
	if regs, _ := store.LoadRegistrations(); len(regs) != 0 {
		t.Fatalf("an excluded workspace registered: %#v", regs)
	}
	if ds, _ := readCaptureDiagnostics(home); len(ds) != 0 {
		t.Fatalf("an excluded workspace recorded diagnostics: %#v", ds)
	}
	if reqs, _ := store.LoadRequests(); len(reqs) != 0 {
		t.Fatalf("an excluded workspace queued requests: %#v", reqs)
	}
	if _, found, _ := store.ArchiveSessionID(excluded); found {
		t.Fatal("an excluded workspace was given an archive ID")
	}
	sub := filepath.Join(parent, "pkg")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := handleHookEvent(home, "cursor", cursorDesktopPayload("beforeSubmitPrompt", "5f3c2a10-0000-4000-8000-00000000dddd", sub, nil), at); err != nil {
		t.Fatal(err)
	}
	if reg := onlyCursorRegistration(t, home); reg.ProjectRoot != parent {
		t.Fatalf("registration = %#v", reg)
	}
}

// While setup's transaction is open, a never-seen chat's first prompt is a
// start that setup swallowed and is explained by the setup-in-progress
// diagnostic; a registered chat's next prompt is not a start and records none.
func TestCursorFirstPromptDuringSetupIsExplainedOnlyForNewChats(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	registered := "5f3c2a10-0000-4000-8000-00000000eeee"
	if err := handleHookEvent(home, "cursor", cursorDesktopPayload("beforeSubmitPrompt", registered, project, nil), at); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journalPath(home), []byte(`{"changes":[],"plist":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := handleHookEvent(home, "cursor", cursorDesktopPayload("beforeSubmitPrompt", registered, project, nil), at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ds, _ := readCaptureDiagnostics(home); len(ds) != 0 {
		t.Fatalf("a registered chat's prompt was reported as a swallowed start: %#v", ds)
	}
	if err := handleHookEvent(home, "cursor", cursorDesktopPayload("beforeSubmitPrompt", "5f3c2a10-0000-4000-8000-00000000ffff", project, nil), at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	ds, _ := readCaptureDiagnostics(home)
	if len(ds) != 1 || ds[0].Code != diagnosticSetupInProgress || ds[0].Harness != "cursor" || ds[0].ProjectRoot != project {
		t.Fatalf("diagnostics = %#v", ds)
	}
	if regs, _ := onlyCursorRegistrationList(t, home); len(regs) != 1 || regs[0].NativeSessionID != registered {
		t.Fatalf("registrations = %#v", regs)
	}
}

func onlyCursorRegistrationList(t *testing.T, home string) ([]archive.SessionRegistration, error) {
	t.Helper()
	store, err := collector.NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	return store.LoadRegistrations()
}
