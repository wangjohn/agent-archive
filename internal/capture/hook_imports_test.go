package capture

import (
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

// A hook admits a new session the moment it starts, and says so.
func TestHookRegistrationRecordsAdmissionAndOrigin(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": "/work/widget", "transcript_path": "/tmp/t.jsonl"}
	if err := HandleEvent(home, "claude", payload, now, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	regs, err := state.OpenReadOnly(home).LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("regs=%#v err=%v", regs, err)
	}
	reg := regs[0]
	if !reg.SessionStartedAt.Equal(now) || !reg.AdmittedAt.Equal(now) || reg.Origin != archive.SessionOriginHook || reg.StartedAtSource != archive.StartedAtSourceHook || !reg.ImportBatch.IsZero() || reg.Imported() {
		t.Fatalf("reg=%#v", reg)
	}
	cfg, _, _ := config.Load(home)
	if reg.DestinationID == "" || reg.DestinationID != cfg.DestinationID() {
		t.Fatalf("destination ID %q, want the configured %q", reg.DestinationID, cfg.DestinationID())
	}
}

// A hook session admitted into bucket A is declined while the destination is
// B, and continues again once it is switched back to A: its objects are
// there. A continuation never rewrites the destination ID.
func TestHookContinuationFollowsTheAdmittedDestination(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	activated := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, "/work/widget", activated)
	start := activated.Add(24 * time.Hour)
	payload := func(path string) map[string]any {
		return map[string]any{"hook_event_name": "SessionStart", "source": "resume", "session_id": "native-1", "cwd": "/work/widget", "transcript_path": path}
	}
	startPayload := payload("/tmp/a.jsonl")
	startPayload["source"] = "startup"
	if err := HandleEvent(home, "claude", startPayload, start, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	regs, _ := store.LoadRegistrations()
	if len(regs) != 1 {
		t.Fatalf("regs=%#v", regs)
	}
	bucketA := regs[0].DestinationID

	cfg, _, _ := config.Load(home)
	storageA := cfg.Storage
	cfg.PreviousDestinations = append(cfg.PreviousDestinations, storageA)
	cfg.Storage.Bucket = "bucket-b"
	cfg.DestinationSince = start.Add(time.Hour)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if err := HandleEvent(home, "claude", payload("/tmp/b.jsonl"), start.Add(2*time.Hour), WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	regs, _ = store.LoadRegistrations()
	if len(regs) != 1 || regs[0].TranscriptPath != "/tmp/a.jsonl" || regs[0].DestinationID != bucketA {
		t.Fatalf("a continuation while the destination is B changed the registration: %#v", regs)
	}

	cfg.Storage = storageA
	cfg.DestinationSince = start.Add(3 * time.Hour)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if err := HandleEvent(home, "claude", payload("/tmp/a2.jsonl"), start.Add(4*time.Hour), WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	regs, _ = store.LoadRegistrations()
	if len(regs) != 1 || regs[0].TranscriptPath != "/tmp/a2.jsonl" || regs[0].DestinationID != bucketA || !regs[0].AdmittedAt.Equal(start) {
		t.Fatalf("the continuation after switching back to A was not accepted as the same session: %#v", regs)
	}
}

// A registration written before destination IDs existed keeps none when a
// hook continues it, and is judged by time as before.
func TestHookContinuationOfLegacyRegistrationKeepsNoDestinationID(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	activated := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, "/work/widget", activated)
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	start := activated.Add(24 * time.Hour)
	if _, err := store.RegisterNewSession(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("claude")), NativeID: "native-1"}, func(id string) archive.SessionRegistration {
		return archive.SessionRegistration{
			ArchiveSessionID: id, NativeSessionID: "native-1", ProjectID: archive.ProjectID("/work/widget"), ProjectRoot: "/work/widget",
			Harness: archive.Harness{Name: "claude"}, TranscriptPath: "/tmp/old.jsonl", SessionStartedAt: start, RegisteredAt: start,
		}
	}); err != nil {
		t.Fatal(err)
	}
	resume := map[string]any{"hook_event_name": "SessionStart", "source": "resume", "session_id": "native-1", "cwd": "/work/widget", "transcript_path": "/tmp/new.jsonl"}
	if err := HandleEvent(home, "claude", resume, start.Add(time.Hour), WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	regs, _ := store.LoadRegistrations()
	if len(regs) != 1 || regs[0].TranscriptPath != "/tmp/new.jsonl" || regs[0].DestinationID != "" {
		t.Fatalf("regs=%#v", regs)
	}
	// Same bucket, but DestinationSince after its start: the time rule, as
	// before, declines it.
	cfg, _, _ := config.Load(home)
	cfg.DestinationSince = start.Add(2 * time.Hour)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	resume["transcript_path"] = "/tmp/newer.jsonl"
	if err := HandleEvent(home, "claude", resume, start.Add(3*time.Hour), WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	if regs, _ := store.LoadRegistrations(); len(regs) != 1 || regs[0].TranscriptPath != "/tmp/new.jsonl" {
		t.Fatalf("a legacy registration before DestinationSince was continued: %#v", regs)
	}
}

// A hook that resumes an imported session continues it: it is accepted,
// keeps the import's start, admission, and provenance, and takes the new
// transcript path.
func TestHookResumeOfImportKeepsProvenanceAndUpdatesPath(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	activated := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, "/work/widget", activated)
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	importedAt := activated.Add(time.Hour)
	imported, err := store.RegisterNewSession(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("claude")), NativeID: "native-1"}, func(id string) archive.SessionRegistration {
		return archive.SessionRegistration{
			ArchiveSessionID: id, NativeSessionID: "native-1", ProjectID: archive.ProjectID("/work/widget"), ProjectRoot: "/work/widget",
			Harness: archive.Harness{Name: "claude"}, TranscriptPath: "/tmp/old.jsonl",
			SessionStartedAt: time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC), RegisteredAt: importedAt,
			AdmittedAt: importedAt, Origin: archive.SessionOriginImport, StartedAtSource: archive.StartedAtSourceTranscript, ImportBatch: archive.NewImportBatch("2026-09-01-1"),
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	resumeAt := importedAt.Add(48 * time.Hour)
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "resume", "session_id": "native-1", "cwd": "/work/widget", "transcript_path": "/tmp/new.jsonl"}
	if err := HandleEvent(home, "claude", payload, resumeAt, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("regs=%#v err=%v", regs, err)
	}
	got := regs[0]
	if got.TranscriptPath != "/tmp/new.jsonl" || !got.RegisteredAt.Equal(resumeAt) {
		t.Fatalf("resume was not accepted: %#v", got)
	}
	want := imported
	want.TranscriptPath, want.RegisteredAt = got.TranscriptPath, got.RegisteredAt
	want.HookObservedAt = resumeAt
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resume changed the import's provenance:\n got %#v\nwant %#v", got, want)
	}
	if requests, _ := store.LoadRequests(); len(requests) != 1 || requests[0].ArchiveSessionID != imported.ArchiveSessionID {
		t.Fatalf("the resume's lifecycle evidence was not queued: %#v", requests)
	}
}

// A missing child identity must not discard the parent hook observation or
// replace the import's original admission and attribution.
func TestSubagentStopDurablyObservesHookOnImportedParent(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	activated := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, "/work/widget", activated)
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	importedAt := activated.Add(time.Hour)
	key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "imported-parent"}
	imported, err := store.RegisterNewSession(key, func(id string) archive.SessionRegistration {
		return archive.SessionRegistration{
			ArchiveSessionID: id, NativeSessionID: key.NativeID,
			ProjectID: archive.ProjectID("/work/widget"), ProjectRoot: "/work/widget",
			Harness: archive.Harness{Name: string(key.Agent)}, TranscriptPath: "/tmp/imported.jsonl",
			SessionStartedAt: activated.Add(-24 * time.Hour), RegisteredAt: importedAt,
			AdmittedAt: importedAt, DestinationID: cfg.DestinationID(),
			Origin: archive.SessionOriginImport, StartedAtSource: archive.StartedAtSourceTranscript,
			ImportBatch: archive.NewImportBatch("2026-09-01-1"),
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	observedAt := importedAt.Add(time.Hour)
	event := agentapi.LifecycleEvent{
		Kind:        agentapi.EventSubagent,
		Reason:      "subagentstop",
		NativeEvent: "SubagentStop",
		Session:     agentapi.NativeSession{Agent: key.Agent, NativeID: key.NativeID},
		ProjectRoot: imported.ProjectRoot,
		Child:       &agentapi.ChildObservation{MissingDetail: "native hook omitted child identity"},
	}
	if err := HandleBatch(home, string(key.Agent), []agentapi.LifecycleEvent{event}, observedAt); err != nil {
		t.Fatal(err)
	}
	// Reopen to check the durable record, rather than an in-memory return value.
	persisted := state.OpenReadOnly(home)
	regs, err := persisted.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("registrations=%#v err=%v", regs, err)
	}
	want := imported
	want.HookObservedAt = observedAt
	if !reflect.DeepEqual(regs[0], want) {
		t.Fatalf("hook observation changed imported attribution:\n got %#v\nwant %#v", regs[0], want)
	}
	candidates, err := persisted.LoadSubagentCandidates()
	if err != nil || len(candidates) != 0 {
		t.Fatalf("candidates=%#v err=%v", candidates, err)
	}
	requests, err := persisted.LoadRequests()
	if err != nil || len(requests) != 1 || requests[0].ArchiveSessionID != imported.ArchiveSessionID {
		t.Fatalf("missing-child evidence was not queued for the imported parent: %#v err=%v", requests, err)
	}
}
