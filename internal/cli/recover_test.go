package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/state/statetest"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func recoverFixture(t *testing.T) (Env, string, string, string) {
	t.Helper()
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	env, home, path, _ := publishedThroughSync(t, at)
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := onlyRegistration(t, home)
	bundle, publishedAt, found, err := store.LoadLastPublished(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(err)
	}
	if err := statetest.SaveBlocked(store, reg.ArchiveSessionID, bundle, publishedAt, state.BlockedReasonTranscriptRewritten); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"turn_context","model":"gpt-test"}
{"type":"response_item","id":"now","payload":{"type":"message","role":"assistant","content":"current recoverable activity"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	env.Now = func() time.Time { return at.Add(time.Hour) }
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { t.Fatal("recovery opened storage"); return nil, nil }
	return env, home, path, reg.ArchiveSessionID
}

func TestRecoverPreviewConfirmAndHookRouting(t *testing.T) {
	t.Parallel()
	env, home, path, id := recoverFixture(t)
	before, err := os.ReadFile(home + "/config.json")
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"recover", id}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("preview exit %d %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "feedback and handoffs") || !strings.Contains(out.String(), "--confirm") {
		t.Fatalf("incomplete preview %s", out.String())
	}
	after, _ := os.ReadFile(home + "/config.json")
	if !bytes.Equal(before, after) {
		t.Fatal("preview changed config")
	}
	store := state.OpenReadOnly(home)
	if _, found, err := store.GenerationSuccessor(id); err != nil || found {
		t.Fatal("preview created recovery", err)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"recover", id, "--confirm"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("confirm exit %d %s", code, errOut.String())
	}
	cfg, _, err := config.Load(home)
	if err != nil || !cfg.GenerationProtection || cfg.SchemaVersion != 4 {
		t.Fatalf("missing downgrade protection %#v %v", cfg, err)
	}
	next, found, err := store.GenerationSuccessor(id)
	if err != nil || !found {
		t.Fatalf("successor missing %v", err)
	}
	for range 2 {
		if code := Run([]string{"recover", id, "--confirm"}, nil, &out, &errOut, env); code != 0 {
			t.Fatalf("repeat %d %s", code, errOut.String())
		}
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 2 {
		t.Fatalf("duplicate registration %#v %v", regs, err)
	}
	at := env.now().Add(time.Minute)
	payload := map[string]any{"hook_event_name": "Stop", "session_id": "native", "cwd": regs[0].ProjectRoot, "transcript_path": path}
	// Use the original project independent of sorted IDs.
	old, _, _ := store.LoadRegistration(id)
	payload["cwd"] = old.ProjectRoot
	if err := handleTestHookEvent(home, "codex", payload, at); err != nil {
		t.Fatal(err)
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "native"}
	active, found, err := store.ArchiveSessionID(key)
	if err != nil || !found || active != next {
		t.Fatalf("hook map %s %v", active, err)
	}
	request, found, err := store.LoadRequest(next)
	if err != nil || !found || request.ArchiveSessionID != next {
		t.Fatalf("hook request %#v %v", request, err)
	}
	if _, found, err := store.LoadRequest(id); err != nil || found {
		t.Fatalf("hook wrote historical request %v", err)
	}
	// Full local reconstruction preserves active mapping after ordinary hooks.
	mutable, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := mutable.MarkSessionIndexRecoveryNeeded(); err != nil {
		t.Fatal(err)
	}
	if _, err := mutable.RecoverSessionIndexScheduled(context.Background(), state.SessionIndexRecoverySlice); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverRefusesPauseDestinationAndSubagent(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"paused", "destination", "subagent", "pending"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			env, home, _, id := recoverFixture(t)
			store, err := state.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			cfg, _, err := config.Load(home)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "paused":
				cfg.Paused = true
			case "destination":
				cfg.Storage.Bucket = "another-destination"
			case "subagent":
				_, err = store.UpdateRegistration(id, func(reg *archive.SessionRegistration) error { reg.ParentSessionID = "parent"; return nil })
				if err != nil {
					t.Fatal(err)
				}
			case "pending":
				if err := store.SavePending(id, state.PendingPublication{SourceKey: "source", MetadataKey: "metadata", SourceSHA256: "sha", SourceBytes: []byte("synthetic"), MetadataBytes: []byte(`{}`)}); err != nil {
					t.Fatal(err)
				}
			}
			if err := config.Save(home, cfg); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			if code := Run([]string{"recover", id, "--confirm"}, nil, &out, &errOut, env); code != 1 {
				t.Fatalf("%s accepted exit %d %s %s", mode, code, out.String(), errOut.String())
			}
			if _, found, err := store.GenerationSuccessor(id); err != nil || found {
				t.Fatal("refusal registered successor", err)
			}
		})
	}
}
