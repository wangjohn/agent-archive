package capture

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestSessionStartPersistsHookObservationOnce(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, "/work/widget", now.Add(-time.Hour))
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	event := agentapi.LifecycleEvent{Kind: agentapi.EventStart, Session: agentapi.NativeSession{Agent: agentmeta.Codex, NativeID: "native-start"}, ProjectRoot: "/work/widget", Start: agentapi.StartEvidence{Kind: agentapi.FreshExplicit}, Reason: "sessionstart"}
	for i := range 2 {
		at := now.Add(time.Duration(i) * time.Minute)
		var persisted os.FileInfo
		var path string
		if err := handleSessionStart(home, store, cfg, event, at, gitLookups{}, func(effect effectName) error {
			if effect == effectRegistrationCreate || effect == effectRegistrationUpdate {
				regs, err := store.LoadRegistrations()
				if err != nil || len(regs) != 1 || !regs[0].HookObservedAt.Equal(at) || regs[0].Origin != archive.SessionOriginHook {
					t.Fatalf("registration not persisted at start boundary: %#v %v", regs, err)
				}
				path = filepath.Join(home, "registrations", regs[0].ArchiveSessionID+".json")
				persisted, err = os.Stat(path)
				return err
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		finished, err := os.Stat(path)
		if err != nil || persisted == nil || !os.SameFile(persisted, finished) {
			t.Fatalf("start registration was replaced again while saving evidence: %v", err)
		}
	}
}
