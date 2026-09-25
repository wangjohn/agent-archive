package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// cursorSetup installs for Cursor only, with one included project.
func cursorSetup(t *testing.T, now time.Time) (Env, string, string, string) {
	t.Helper()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", false, false, true, project), 0)
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Harnesses) != 1 || cfg.Harnesses[0] != "cursor" || len(cfg.Archive.Projects) != 1 {
		t.Fatalf("setup: harnesses=%v projects=%v", cfg.Harnesses, cfg.Archive.Projects)
	}
	return env, home, userHome, cfg.Archive.Projects[0].Root
}

func changeBucket(t *testing.T, env Env, home, userHome string) (config.Config, string, error) {
	t.Helper()
	return changeBucketTo(t, env, home, userHome, "another-bucket")
}

func changeBucketTo(t *testing.T, env Env, home, userHome, bucket string) (config.Config, string, error) {
	t.Helper()
	old, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	next := old
	next.Storage.Bucket = bucket
	var out strings.Builder
	if err := reviewChanges(home, old, next, newPrompter(strings.NewReader(""), &out), env); err != nil {
		return config.Config{}, out.String(), err
	}
	exe, err := env.executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := applySetup(home, userHome, exe, old, &next, nil, env); err != nil {
		return config.Config{}, out.String(), err
	}
	committed, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	return committed, out.String(), nil
}

// A Cursor chat whose transcript never arrives (transcripts turned off) keeps
// a queued request forever. It has nothing a sync could publish, so it must
// not pin the user to the current destination. It still shows as pending in
// status, setup says what happens to it, and after the change it falls behind
// the new boundary like any unpublished session.
func TestWaitingCursorChatDoesNotBlockADestinationChange(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	env, home, userHome, project := cursorSetup(t, now)
	conversation := "5f3c2a10-0000-4000-8000-00000000cccc"
	for _, event := range []string{"beforeSubmitPrompt", "afterAgentResponse", "stop"} {
		if err := capture.HandleEvent(home, "cursor", cursorDesktopPayload(event, conversation, project, nil), now.Add(time.Minute)); err != nil {
			t.Fatalf("%s: %v", event, err)
		}
	}
	reg := onlyCursorRegistration(t, home)
	if reg.TranscriptPath != "" {
		t.Fatalf("test precondition: the chat should have no transcript path: %#v", reg)
	}
	cfg, _, _ := config.Load(home)
	if pending, err := pendingSessions(home, cfg); err != nil || pending != 1 {
		t.Fatalf("status must still count the waiting chat: pending=%d err=%v", pending, err)
	}

	// The user changes storage an hour after the chat started.
	env.Now = func() time.Time { return now.Add(time.Hour) }
	committed, out, err := changeBucket(t, env, home, userHome)
	if err != nil {
		t.Fatalf("a waiting chat blocked the destination change: %v", err)
	}
	if !strings.Contains(out, "1 session(s) never received a transcript") || !strings.Contains(out, "will not be captured at the new destination") {
		t.Fatalf("setup did not explain the waiting chat: %s", out)
	}
	if committed.Storage.Bucket != "another-bucket" || committed.DestinationSince.IsZero() {
		t.Fatalf("destination not changed: %+v", committed.Storage)
	}
	if committed.AcceptSession(reg) {
		t.Fatal("the waiting chat was carried across the new boundary")
	}
	if pending, err := pendingSessions(home, committed); err != nil || pending != 0 {
		t.Fatalf("the left-behind chat still counts as pending: %d err=%v", pending, err)
	}
}

// A chat that has its transcript and has not published yet is real pending
// work; it still blocks a destination change until it is synced.
func TestCursorChatWithATranscriptStillBlocksADestinationChange(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	env, home, userHome, project := cursorSetup(t, now)
	conversation := "5f3c2a10-0000-4000-8000-00000000dddd"
	transcript := cursorTranscriptLocation(t, conversation)
	if err := capture.HandleEvent(home, "cursor", cursorDesktopPayload("beforeSubmitPrompt", conversation, project, nil), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte(`{"role":"user","message":{"content":[{"type":"text","text":"hi"}]}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := capture.HandleEvent(home, "cursor", cursorDesktopPayload("stop", conversation, project, transcript), now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if reg := onlyCursorRegistration(t, home); reg.TranscriptPath != transcript {
		t.Fatalf("test precondition: the chat should have its transcript: %#v", reg)
	}
	if _, _, err := changeBucket(t, env, home, userHome); err == nil || !strings.Contains(err.Error(), "still pending") {
		t.Fatalf("pending work did not block the change: %v", err)
	}
}

// A session admitted into bucket A records it. While the destination is B it
// is not accepted and does not hold setup at B; switching back to A accepts
// it again, pending work included, because its objects are in A.
func TestSwitchingBackToADestinationAcceptsItsSessionsAgain(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	env, home, userHome, project := cursorSetup(t, now)
	conversation := "5f3c2a10-0000-4000-8000-00000000eeee"
	for _, event := range []string{"beforeSubmitPrompt", "stop"} {
		if err := capture.HandleEvent(home, "cursor", cursorDesktopPayload(event, conversation, project, nil), now.Add(time.Minute)); err != nil {
			t.Fatalf("%s: %v", event, err)
		}
	}
	reg := onlyCursorRegistration(t, home)
	env.Now = func() time.Time { return now.Add(time.Hour) }
	atB, out, err := changeBucketTo(t, env, home, userHome, "bucket-b")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "resume uploading") {
		t.Fatalf("a new destination was said to resume sessions: %s", out)
	}
	if atB.AcceptSession(reg) || atB.InCurrentDestination(reg) {
		t.Fatal("a session admitted into A was accepted at B")
	}
	env.Now = func() time.Time { return now.Add(2 * time.Hour) }
	backAtA, out, err := changeBucketTo(t, env, home, userHome, "test-bucket")
	if err != nil {
		t.Fatalf("a session of A blocked switching back to A: %v", err)
	}
	if !strings.Contains(out, "1 session(s) from when this destination was used before resume uploading there") {
		t.Fatalf("setup did not say A's session resumes: %s", out)
	}
	if !backAtA.DestinationSince.After(reg.Admitted()) {
		t.Fatalf("test precondition: the switch back moved DestinationSince past the admission: %v", backAtA.DestinationSince)
	}
	if !backAtA.AcceptSession(reg) || !backAtA.InCurrentDestination(reg) {
		t.Fatal("a session admitted into A was not accepted after switching back to A")
	}
	if pending, err := pendingSessions(home, backAtA); err != nil || pending != 1 {
		t.Fatalf("its outstanding work is not pending again at A: %d err=%v", pending, err)
	}
}

// An imported Cursor database chat has no transcript path by design, but it
// is not waiting for one: a sync reads it from Cursor's database. Until it
// is uploaded it is pending, and it blocks a destination change like any
// pending session rather than being left behind with a wrong warning.
func TestImportedCursorDatabaseChatBlocksADestinationChange(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	env, home, userHome, project := cursorSetup(t, now)
	cfg, _, _ := config.Load(home)
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	admitted := now.Add(time.Minute)
	reg, err := store.RegisterNewSession("db-chat", func(id string) archive.SessionRegistration {
		return archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: "db-chat", ProjectID: archive.ProjectID(project), ProjectRoot: project,
			Harness: archive.Harness{Name: "cursor"}, SourceKind: archive.SourceKindCursorSQLite, SourceKey: "db-chat",
			SessionStartedAt: now.Add(-time.Hour), StartedAtSource: archive.StartedAtSourceCursorComposer,
			RegisteredAt: admitted, AdmittedAt: admitted, Origin: archive.SessionOriginImport, ImportBatch: archive.NewImportBatch("2026-09-23-1"), DestinationID: cfg.DestinationID()}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRequest(reg.ArchiveSessionID, "backfill", admitted); err != nil {
		t.Fatal(err)
	}
	if blocking, waiting, err := pendingSessionCounts(home, cfg); err != nil || blocking != 1 || waiting != 0 {
		t.Fatalf("blocking %d, waiting %d, err %v", blocking, waiting, err)
	}
	env.Now = func() time.Time { return now.Add(time.Hour) }
	if _, out, err := changeBucket(t, env, home, userHome); err == nil || !strings.Contains(err.Error(), "still pending") || strings.Contains(out, "never received a transcript") {
		t.Fatalf("the destination changed under an un-uploaded import: %v\n%s", err, out)
	}
}

type failingUpdateStore struct{ storage.ObjectStore }

func (s failingUpdateStore) Put(context.Context, string, []byte) error { return errors.New("offline") }

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestFailedScheduledUpdateBlocksDestinationSwitchUntilRetry(t *testing.T) {
	t.Parallel()
	home, dir := t.TempDir(), t.TempDir()
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, dir, now.Add(-time.Hour))
	path := writeCodexTranscript(t, dir)
	if err := capture.HandleEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native", "cwd": dir, "transcript_path": path}, now); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	ls, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	cloud := storagetest.NewMemoryStore()
	opts := collector.Options{MachineID: cfg.MachineID, Now: func() time.Time { return now }, Retry: storage.RetryPolicy{MaxAttempts: 1}}
	run := func(store storage.ObjectStore, fail bool) {
		t.Helper()
		r, e := collector.Run(context.Background(), ls, store, opts)
		if e != nil || (len(r.Errors) > 0) != fail {
			t.Fatalf("result=%+v err=%v", r, e)
		}
	}
	run(cloud, false)
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	update := `{"type":"response_item","id":"update","payload":{"type":"message","role":"user","content":"updated"}}`
	if err := os.WriteFile(path, append(append(bytes, '\n'), update...), 0600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	run(failingUpdateStore{cloud}, true)
	// Reopen the store through pendingSessions to verify persistence across runs.
	pending, err := pendingSessions(home, cfg)
	if err != nil || pending != 1 {
		t.Fatalf("pending=%d err=%v", pending, err)
	}
	next := cfg
	next.Storage.Bucket = "another-bucket"
	if err := reviewChanges(home, cfg, next, newPrompter(strings.NewReader(""), os.Stdout), testEnv(t, home, now)); err == nil {
		t.Fatal("destination switch allowed with a failed update")
	}
	run(cloud, false)
	pending, err = pendingSessions(home, cfg)
	if err != nil || pending != 0 {
		t.Fatalf("pending after retry=%d err=%v", pending, err)
	}
}
