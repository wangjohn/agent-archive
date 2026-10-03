package capture

import (
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestRealHookPreservesDiscoveryLocatorAndRecordsObservation(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, "/work/widget", start.Add(-time.Hour))
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "native-1"}
	original, err := store.RegisterNewSession(key, func(id string) archive.SessionRegistration {
		return archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: key.NativeID, Harness: archive.Harness{Name: "codex"}, ProjectID: archive.ProjectID("/work/widget"), ProjectRoot: "/work/widget", TranscriptPath: "/approved/rollout.jsonl", DiscoveryRoot: "/approved", DiscoveryCwd: "/work/widget", SessionStartedAt: start, AdmittedAt: start, RegisteredAt: start, Origin: archive.SessionOriginDiscovery, DestinationID: cfg.DestinationID()}
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, event := range []string{"SessionStart", "UserPromptSubmit", "Stop"} {
		at := start.Add(time.Duration(i+1) * time.Minute)
		payload := map[string]any{"hook_event_name": event, "source": "resume", "session_id": key.NativeID, "cwd": "/work/widget", "transcript_path": "/outside/rollout.jsonl"}
		if err := HandleEvent(home, "codex", payload, at, WithDecoders(testDecoders)); err != nil {
			t.Fatal(err)
		}
		current, found, err := store.LoadRegistration(original.ArchiveSessionID)
		if err != nil || !found || current.TranscriptPath != original.TranscriptPath || current.DiscoveryRoot != original.DiscoveryRoot || current.Origin != original.Origin || !current.AdmittedAt.Equal(original.AdmittedAt) || !current.HookObservedAt.Equal(at) {
			t.Fatalf("%s changed discovery ownership or lost hook observation: %#v, %v", event, current, err)
		}
	}
}
