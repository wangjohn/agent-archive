package capture

import (
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/state"
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
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("registrations = %#v err=%v", regs, err)
	}
	return regs[0]
}

// A resumed chat's first beforeSubmitPrompt already names its non-empty
// transcript: that proves nothing about when the chat began, so it is declined
// with the existing diagnostic.
func TestCursorResumedChatIsDeclinedAtFirstPrompt(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	conversation := "5f3c2a10-0000-4000-8000-00000000d0d0"
	transcript := cursorTranscriptLocation(t, conversation)
	if err := os.WriteFile(transcript, []byte(`{"role":"user","message":{"content":[{"type":"text","text":"earlier"}]}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	for _, event := range []string{"beforeSubmitPrompt", "afterAgentResponse", "stop", "sessionEnd"} {
		if err := HandleEvent(home, "cursor", cursorDesktopPayload(event, conversation, project, transcript), at, WithDecoders(testDecoders)); err != nil {
			t.Fatal(err)
		}
	}
	store, _ := state.Open(home)
	if regs, _ := store.LoadRegistrations(); len(regs) != 0 {
		t.Fatalf("a resumed chat was registered: %#v", regs)
	}
	ds, _ := ReadDiagnostics(home)
	if len(ds) != 1 || ds[0].Code != DiagnosticUnknownSessionStart || ds[0].Harness != "cursor" || ds[0].ProjectRoot != project {
		t.Fatalf("diagnostics = %#v", ds)
	}

	// Outside every configured project nothing is recorded at all.
	outside := t.TempDir()
	other := cursorDesktopPayload("beforeSubmitPrompt", "other-conversation", outside, transcript)
	if err := HandleEvent(home, "cursor", other, at, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	if after, _ := ReadDiagnostics(home); len(after) != 1 {
		t.Fatalf("a path outside every project was recorded: %#v", after)
	}
}

// Only a path that is absolute and named <conversation_id>.jsonl is taken as
// the chat's transcript, and once set it is never replaced.
func TestCursorTranscriptPathMustMatchTheConversation(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	conversation := "5f3c2a10-0000-4000-8000-00000000e0e0"
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	if err := HandleEvent(home, "cursor", cursorDesktopPayload("beforeSubmitPrompt", conversation, project, nil), at, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	wrongName := filepath.Join(filepath.Dir(cursorTranscriptLocation(t, conversation)), "another-conversation.jsonl")
	for _, path := range []string{wrongName, "relative/" + conversation + ".jsonl", filepath.Join(t.TempDir(), conversation+".txt")} {
		if err := HandleEvent(home, "cursor", cursorDesktopPayload("afterAgentResponse", conversation, project, path), at.Add(time.Second), WithDecoders(testDecoders)); err != nil {
			t.Fatal(err)
		}
		if reg := onlyCursorRegistration(t, home); reg.TranscriptPath != "" {
			t.Fatalf("path %q was adopted: %#v", path, reg)
		}
	}
	right := cursorTranscriptLocation(t, conversation)
	if err := HandleEvent(home, "cursor", cursorDesktopPayload("stop", conversation, project, right), at.Add(2*time.Second), WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	if reg := onlyCursorRegistration(t, home); reg.TranscriptPath != right {
		t.Fatalf("matching path not adopted: %#v", reg)
	}
	elsewhere := cursorTranscriptLocation(t, conversation)
	for _, event := range []string{"beforeSubmitPrompt", "afterAgentResponse", "stop", "sessionStart"} {
		if err := HandleEvent(home, "cursor", cursorDesktopPayload(event, conversation, project, elsewhere), at.Add(3*time.Second), WithDecoders(testDecoders)); err != nil {
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
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	for harness, event := range map[string]string{"codex": "UserPromptSubmit", "claude": "UserPromptSubmit"} {
		payload := map[string]any{"hook_event_name": event, "session_id": harness + "-session", "cwd": project}
		if err := HandleEvent(home, harness, payload, at, WithDecoders(testDecoders)); err != nil {
			t.Fatal(err)
		}
		start := map[string]any{"hook_event_name": "SessionStart", "session_id": harness + "-start", "cwd": project}
		if err := HandleEvent(home, harness, start, at, WithDecoders(testDecoders)); err != nil {
			t.Fatal(err)
		}
	}
	store, _ := state.Open(home)
	if regs, _ := store.LoadRegistrations(); len(regs) != 0 {
		t.Fatalf("a non-Cursor session registered without its documented start proof: %#v", regs)
	}
}

// Hooks for one new chat can overlap: a first prompt, a fast response, and a
// stop. A 100 ms hold under hooks.lock models a slow durable write per event.
// The bounded CI load target is 3 hooks x 10 rounds, zero missing
// registrations, and a p99 hook latency below the installed 2 second timeout.
func TestCursorOverlappingHooksRegisterOnce(t *testing.T) {
	// Not parallel: this is a wall-clock acceptance test.
	slowWrite := os.Getenv("AGENT_ARCHIVE_PERF") == "1"
	var durations []time.Duration
	for i := range 10 {
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
		times := make(chan time.Duration, 3)
		for _, event := range []string{"beforeSubmitPrompt", "afterAgentResponse", "stop"} {
			wg.Add(1)
			go func(event string) {
				defer wg.Done()
				var path any = transcript
				if event == "beforeSubmitPrompt" {
					path = nil
				}
				start := time.Now()
				var afterLock func()
				if slowWrite {
					afterLock = func() { time.Sleep(100 * time.Millisecond) }
				}
				errs <- handleEvent(home, "cursor", cursorDesktopPayload(event, conversation, project, path), at, nil, afterLock)
				times <- time.Since(start)
			}(event)
		}
		wg.Wait()
		close(errs)
		close(times)
		for elapsed := range times {
			durations = append(durations, elapsed)
		}
		overloaded := false
		stderrExplained := false
		for err := range errs {
			if err == nil {
				continue
			}
			if strings.Contains(err.Error(), "status may not show this missed hook") {
				stderrExplained = true
			}
			if strings.Contains(err.Error(), "this hook was not retained") || strings.Contains(err.Error(), "this hook was not recorded") || strings.Contains(err.Error(), "admission queued: false") {
				// The ordinary race suite can run alongside unrelated I/O.
				// An event that misses both locks is outside the bounded CI
				// performance target. It must leave a diagnostic or stderr
				// explanation; the dedicated performance run remains strict.
				if slowWrite {
					t.Fatal(err)
				}
				overloaded = true
				continue
			}
			// A queued event can be durable even when the separate status
			// diagnostic lock times out. That case is reported on hook stderr;
			// the replay assertions below still require every event's effect.
			if !strings.Contains(err.Error(), "status may not show this missed hook") {
				t.Fatal(err)
			}
		}
		if err := ReplayAdmissionIntents(home, at.Add(time.Second), testDecoders); err != nil {
			t.Fatal(err)
		}
		if overloaded {
			diagnostics, err := ReadDiagnostics(home)
			if err != nil {
				t.Fatal(err)
			}
			visible := stderrExplained
			for _, diagnostic := range diagnostics {
				visible = visible || diagnostic.Code == DiagnosticHookBusy
			}
			if !visible {
				t.Fatal("unretained hook had neither hook_busy diagnostic nor stderr explanation")
			}
			continue
		}
		store, _ := state.Open(home)
		regs, err := store.LoadRegistrations()
		if err != nil || len(regs) != 1 {
			t.Fatalf("iteration %d: registrations = %#v err=%v", i, regs, err)
		}
		id, found, err := store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("cursor")), NativeID: conversation})
		if err != nil || !found || id != regs[0].ArchiveSessionID || !regs[0].SessionStartedAt.Equal(at) {
			t.Fatalf("iteration %d: index=%q found=%v err=%v registration=%#v", i, id, found, err, regs[0])
		}
		if regs[0].TranscriptPath != transcript {
			t.Fatalf("iteration %d: transcript path was lost: %#v", i, regs[0])
		}
		if requests, err := store.LoadRequests(); err != nil || len(requests) != 1 ||
			!slices.Contains(requests[0].Reasons, "afteragentresponse") || !slices.Contains(requests[0].Reasons, "stop") {
			t.Fatalf("iteration %d: response/stop request was lost: %#v, %v", i, requests, err)
		}
	}
	if slowWrite {
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		p95, p99 := durations[28], durations[29]
		t.Logf("3-hook x10 slow-write burst: missing registrations=0 p95=%s p99=%s", p95, p99)
		if p99 >= 2*time.Second {
			t.Fatalf("p99 hook latency %s exceeds the 2s app timeout", p99)
		}
	}
}

func TestCursorResponseBeforeFirstPromptPreservesTranscript(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	conversation := "5f3c2a10-0000-4000-8000-00000000c456"
	transcript := cursorTranscriptLocation(t, conversation)
	if err := HandleEvent(home, "cursor", cursorDesktopPayload("afterAgentResponse", conversation, project, transcript), at, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	if regs, err := state.OpenReadOnly(home).LoadRegistrations(); err != nil || len(regs) != 0 {
		t.Fatalf("response admitted session: %#v, %v", regs, err)
	}
	if err := HandleEvent(home, "cursor", cursorDesktopPayload("beforeSubmitPrompt", conversation, project, nil), at, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	if err := ReplayAdmissionIntents(home, at.Add(time.Second), testDecoders); err != nil {
		t.Fatal(err)
	}
	reg := onlyCursorRegistration(t, home)
	if reg.TranscriptPath != transcript {
		t.Fatalf("response path not adopted: %#v", reg)
	}
	store := state.OpenReadOnly(home)
	if requests, err := store.LoadRequests(); err != nil || len(requests) != 1 {
		t.Fatalf("response request was lost: %#v, %v", requests, err)
	}
}

// The user comes back to a chat later. Its next beforeSubmitPrompt names the
// now non-empty transcript; that is a continuation of the registered chat,
// which keeps its start time and takes the path, and no diagnostic is
// recorded. A sessionStart on the same chat is a continuation too.
func TestCursorLaterPromptContinuesTheRegisteredChat(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	first := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	conversation := "5f3c2a10-0000-4000-8000-00000000bbbb"
	transcript := cursorTranscriptLocation(t, conversation)
	if err := HandleEvent(home, "cursor", cursorDesktopPayload("beforeSubmitPrompt", conversation, project, nil), first, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte(`{"role":"user","message":{"content":[{"type":"text","text":"hi"}]}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := first.Add(time.Hour)
	if err := HandleEvent(home, "cursor", cursorDesktopPayload("beforeSubmitPrompt", conversation, project, transcript), later, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	reg := onlyCursorRegistration(t, home)
	if !reg.SessionStartedAt.Equal(first) || reg.TranscriptPath != transcript {
		t.Fatalf("registration = %#v", reg)
	}
	if ds, _ := ReadDiagnostics(home); len(ds) != 0 {
		t.Fatalf("a continuation recorded diagnostics: %#v", ds)
	}
	other := cursorTranscriptLocation(t, conversation)
	if err := HandleEvent(home, "cursor", cursorDesktopPayload("sessionStart", conversation, project, other), later.Add(time.Hour), WithDecoders(testDecoders)); err != nil {
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
	t.Parallel()
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
		if err := HandleEvent(home, "cursor", cursorDesktopPayload(event, excluded, filepath.Join(nested, "sub"), path), at, WithDecoders(testDecoders)); err != nil {
			t.Fatal(err)
		}
	}
	store, _ := state.Open(home)
	if regs, _ := store.LoadRegistrations(); len(regs) != 0 {
		t.Fatalf("an excluded workspace registered: %#v", regs)
	}
	if ds, _ := ReadDiagnostics(home); len(ds) != 0 {
		t.Fatalf("an excluded workspace recorded diagnostics: %#v", ds)
	}
	if reqs, _ := store.LoadRequests(); len(reqs) != 0 {
		t.Fatalf("an excluded workspace queued requests: %#v", reqs)
	}
	if _, found, _ := store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("cursor")), NativeID: excluded}); found {
		t.Fatal("an excluded workspace was given an archive ID")
	}
	sub := filepath.Join(parent, "pkg")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := HandleEvent(home, "cursor", cursorDesktopPayload("beforeSubmitPrompt", "5f3c2a10-0000-4000-8000-00000000dddd", sub, nil), at, WithDecoders(testDecoders)); err != nil {
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
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	registered := "5f3c2a10-0000-4000-8000-00000000eeee"
	if err := HandleEvent(home, "cursor", cursorDesktopPayload("beforeSubmitPrompt", registered, project, nil), at, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(setupjournal.JournalPath(home), []byte(`{"changes":[],"plist":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := HandleEvent(home, "cursor", cursorDesktopPayload("beforeSubmitPrompt", registered, project, nil), at.Add(time.Minute), WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	if ds, _ := ReadDiagnostics(home); len(ds) != 0 {
		t.Fatalf("a registered chat's prompt was reported as a swallowed start: %#v", ds)
	}
	if err := HandleEvent(home, "cursor", cursorDesktopPayload("beforeSubmitPrompt", "5f3c2a10-0000-4000-8000-00000000ffff", project, nil), at.Add(time.Minute), WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	ds, _ := ReadDiagnostics(home)
	if len(ds) != 1 || ds[0].Code != DiagnosticSetupInProgress || ds[0].Harness != "cursor" || ds[0].ProjectRoot != project {
		t.Fatalf("diagnostics = %#v", ds)
	}
	if regs, _ := onlyCursorRegistrationList(t, home); len(regs) != 1 || regs[0].NativeSessionID != registered {
		t.Fatalf("registrations = %#v", regs)
	}
}

func onlyCursorRegistrationList(t *testing.T, home string) ([]archive.SessionRegistration, error) {
	t.Helper()
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	return store.LoadRegistrations()
}

// A chat read from Cursor's database never adopts a transcript path a hook
// reports, at a prompt, a response, a stop, or a continuation: switching
// sources would change its format mid-session.
func TestCursorDatabaseSessionNeverAdoptsTranscriptPath(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	conversation := "5f3c2a10-0000-4000-8000-00000000db01"
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RegisterNewSession(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("cursor")), NativeID: conversation}, func(id string) archive.SessionRegistration {
		return archive.SessionRegistration{
			ArchiveSessionID: id, NativeSessionID: conversation,
			ProjectID: archive.ProjectID(project), ProjectRoot: project,
			Harness: archive.Harness{Name: "cursor"}, SessionStartedAt: at, RegisteredAt: at, AdmittedAt: at,
			Origin: archive.SessionOriginImport, SourceKind: archive.SourceKindCursorSQLite, SourceKey: conversation,
		}
	}); err != nil {
		t.Fatal(err)
	}
	transcript := cursorTranscriptLocation(t, conversation)
	for i, event := range []string{"beforeSubmitPrompt", "afterAgentResponse", "stop", "sessionStart"} {
		when := at.Add(time.Duration(i+1) * time.Minute)
		if err := HandleEvent(home, "cursor", cursorDesktopPayload(event, conversation, project, transcript), when, WithDecoders(testDecoders)); err != nil {
			t.Fatalf("%s: %v", event, err)
		}
		reg := onlyCursorRegistration(t, home)
		if reg.TranscriptPath != "" || reg.SourceKind != archive.SourceKindCursorSQLite || reg.SourceKey != conversation {
			t.Fatalf("%s changed the source: %#v", event, reg)
		}
		if event == "sessionStart" && !reg.RegisteredAt.Equal(when) {
			t.Fatalf("the continuation did not run: %#v", reg)
		}
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestCursorVersionDoesNotProveSessionStart(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Now().UTC()
	setUpTestConfig(t, home, "/work/widget", at.Add(-time.Hour))
	// A resumed chat's transcript already has bytes; an arbitrary version
	// beside it must not turn that into proof of a fresh start.
	resumed := writeTestTranscript(t, "old.jsonl", "{\"role\":\"user\"}\n")
	if err := HandleEvent(home, "cursor", map[string]any{"hook_event_name": "sessionStart", "conversation_id": "old", "workspace_roots": []any{"/work/widget"}, "cursor_version": "99.0.0", "transcript_path": resumed}, at, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 0 {
		t.Fatal("arbitrary version enrolled session")
	}
	ds, _ := ReadDiagnostics(home)
	if len(ds) != 1 || ds[0].Code != DiagnosticUnknownSessionStart {
		t.Fatalf("diagnostics %+v", ds)
	}
}
