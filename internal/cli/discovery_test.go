package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiscoveryAttemptPrecedesUnavailableStorage(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	env := testEnv(t, home, at)
	setUpTestConfig(t, home, t.TempDir(), at.Add(-time.Hour))
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Harnesses = []string{"codex"}
	cfg.Discovery = &config.DiscoveryConfig{Enabled: true, CodexHomes: []string{t.TempDir()}}
	if err := config.ReconcileDiscovery(&cfg, config.Config{}, at); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return nil, errors.New("synthetic outage") }
	if _, err := runOnePass(env, false); err == nil {
		t.Fatal("outage not reported")
	}
	var catalog struct {
		Health discovery.Health `json:"health"`
	}
	if err := local.Read(filepath.Join(home, "discovery-catalog.json"), &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.Health.LastAttempt.IsZero() {
		t.Fatal("storage initialization prevented discovery")
	}
}

func TestDiscoveryStatusDoesNotInventHookObservation(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, t.TempDir(), at.Add(-time.Hour))
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	reg := archive.SessionRegistration{ArchiveSessionID: "discovered", NativeSessionID: "synthetic", ProjectID: archive.ProjectID(cfg.Archive.Projects[0].Root), ProjectRoot: cfg.Archive.Projects[0].Root, Harness: archive.Harness{Name: "codex"}, SessionStartedAt: at, AdmittedAt: at, RegisteredAt: at, Origin: archive.SessionOriginDiscovery, TranscriptPath: filepath.Join(t.TempDir(), "missing.jsonl")}
	if err := store.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	view := statusView{}
	sessions := readSessionStatus(&view, cfg, home, store)
	app := appStatus{}
	pair := projectCaptureStatus{}
	var outcome verificationOutcome
	sessions.addSession(&app, &pair, reg, cfg, home, nil, &outcome)
	if app.HookObserved || pair.HookObserved {
		t.Fatal("discovery invented hook observation")
	}
	const futureOrigin archive.SessionOrigin = "future-source"
	reg.Origin = futureOrigin
	unknownApp, unknownPair := appStatus{}, projectCaptureStatus{}
	sessions.addSession(&unknownApp, &unknownPair, reg, cfg, home, nil, &outcome)
	if unknownApp.HookObserved || unknownPair.HookObserved {
		t.Fatal("unknown source invented hook observation")
	}
	reg.Origin = archive.SessionOriginDiscovery
	reg.HookObservedAt = at.Add(time.Minute)
	sessions.addSession(&app, &pair, reg, cfg, home, nil, &outcome)
	if !app.HookObserved || !pair.HookObserved {
		t.Fatal("actual hook observation lost")
	}
}

// Production adapters and scheduled publication are exercised without a hook
// or a test-only source-support override. The transcript is wholly synthetic.
func TestSupportedDiscoveryPublishesWithoutHooksAndAcceptsRecentCopy(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	physicalTemp := func() string {
		path, err := filepath.EvalSymlinks(t.TempDir())
		must(t, err)
		return path
	}
	home, userHome, project, source := physicalTemp(), physicalTemp(), physicalTemp(), physicalTemp()
	cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: at}}}, Discovery: &config.DiscoveryConfig{Enabled: true, CodexHomes: []string{source}}}
	must(t, config.ReconcileDiscovery(&cfg, config.Config{}, at))
	must(t, config.Save(home, cfg))
	_, err := config.SetPaused(home, true, at.Add(20*time.Second))
	must(t, err)
	cfg, err = config.SetPaused(home, false, at.Add(30*time.Second))
	must(t, err)
	for n, created := range []time.Time{at.Add(time.Minute), at.Add(-time.Hour), at.Add(25 * time.Second)} {
		id := fmt.Sprintf("00000000-0000-0000-0000-%012d", n+1)
		path := filepath.Join(source, "sessions", "rollout-2026-10-02T12-00-00-"+id+".jsonl")
		must(t, os.MkdirAll(filepath.Dir(path), 0700))
		meta, err := json.Marshal(map[string]any{"type": "session_meta", "timestamp": created.Format(time.RFC3339Nano), "payload": map[string]any{"id": id, "timestamp": created.Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "codex-tui", "cli_version": "0.159.3", "history_mode": "paginated"}})
		must(t, err)
		task, err := json.Marshal(map[string]any{"type": "event_msg", "timestamp": created.Format(time.RFC3339Nano), "payload": map[string]any{"type": "task_started", "turn_id": id, "root_turn_id": id, "started_at": created.Format(time.RFC3339Nano)}})
		must(t, err)
		prompt := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic task contains API_KEY=sk-abcdefghijklmnopqrstuvwxyz123456"}]}}`
		original := filepath.Join(t.TempDir(), "copied-native.jsonl")
		must(t, os.WriteFile(original, []byte(string(meta)+"\n"+string(task)+"\n"+prompt+"\n"), 0600))
		copied, err := os.ReadFile(original)
		must(t, err)
		must(t, os.WriteFile(path, copied, 0600))
	}
	remote := storagetest.NewMemoryStore()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at.Add(2*time.Minute))
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	for range 2 {
		result, err := runOnePass(env, true)
		if err != nil || len(result.Errors) > 0 {
			t.Fatalf("scheduled discovery: %+v %v", result, err)
		}
	}
	reg := theRegistration(t, home)
	if reg.Origin != archive.SessionOriginDiscovery || !reg.HookObservedAt.IsZero() || !reg.SessionStartedAt.Equal(at.Add(time.Minute)) {
		t.Fatalf("wrong source provenance: %+v", reg)
	}
	archived := filepath.Join(source, "archived_sessions", filepath.Base(reg.TranscriptPath))
	must(t, os.MkdirAll(filepath.Dir(archived), 0700))
	moved, err := os.ReadFile(reg.TranscriptPath)
	must(t, err)
	must(t, os.WriteFile(archived, moved, 0600))
	must(t, os.Remove(reg.TranscriptPath))
	for range 2 {
		result, err := runOnePass(env, true)
		if err != nil || len(result.Errors) > 0 {
			t.Fatalf("archived move: %+v %v", result, err)
		}
	}
	after := theRegistration(t, home)
	if after.ArchiveSessionID != reg.ArchiveSessionID || after.NativeSessionID != reg.NativeSessionID || after.Origin != reg.Origin || !after.AdmittedAt.Equal(reg.AdmittedAt) {
		t.Fatal("archived copy changed identity or consent")
	}
	evidence, err := readVerification(home, reg.ArchiveSessionID)
	if err != nil || evidence.VerifiedAt.IsZero() {
		t.Fatalf("readback: %+v %v", evidence, err)
	}
	objects, err := remote.List(context.Background(), "")
	must(t, err)
	for _, object := range objects {
		body, err := remote.Get(context.Background(), object.Key)
		must(t, err)
		if strings.HasSuffix(object.Key, ".gz") {
			reader, err := gzip.NewReader(bytes.NewReader(body))
			must(t, err)
			body, err = io.ReadAll(reader)
			must(t, err)
			must(t, reader.Close())
		}
		if strings.Contains(string(body), "sk-abcdefghijklmnopqrstuvwxyz123456") {
			t.Fatal("raw secret reached storage")
		}
	}
	view, err := readStatus(env)
	must(t, err)
	for _, app := range view.Apps {
		if app.Name == "codex" && (app.HookObserved || !app.ReadBackVerified) {
			t.Fatalf("discovery/readback status mixed with hook evidence: %+v", app)
		}
	}
	// File copying only changes arrival time; old creation still fails consent.
	health, found, err := discovery.ReadHealth(home)
	must(t, err)
	if !found || !health.Supported {
		t.Fatalf("real producer registry did not support fixture: %+v", health)
	}
}

func TestDiscoveryFailureSurvivesSuccessfulCollector(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	env := testEnv(t, home, at)
	setUpTestConfig(t, home, t.TempDir(), at.Add(-time.Hour))
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Harnesses = []string{"codex"}
	cfg.Discovery = &config.DiscoveryConfig{Enabled: true, CodexHomes: []string{t.TempDir()}}
	if err := config.ReconcileDiscovery(&cfg, config.Config{}, at); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	// A directory at the catalog path deterministically prevents atomic writes,
	// including when tests run with root privileges.
	if err := os.Mkdir(filepath.Join(home, "discovery-catalog.json"), 0700); err != nil {
		t.Fatal(err)
	}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return storagetest.NewMemoryStore(), nil }
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	status, err := state.OpenReadOnly(home).LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(status.LastErrors, "\n"), "discovery state write failed") {
		t.Fatalf("discovery failure lost: %#v", status.LastErrors)
	}
}

func TestRecoveryPreflightFailureSurvivesLaterSuccessfulCollector(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	env := testEnv(t, home, at)
	setUpTestConfig(t, home, t.TempDir(), at.Add(-time.Hour))
	// Discovery is disabled. A transient hook writer blocks the first census,
	// then releases its lock before the collector's later recovery succeeds.
	unlock, err := local.NamedLock(home, "hooks.lock")
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			unlock()
		}
	}()
	opened := false
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		opened = true
		status, err := state.OpenReadOnly(home).LoadStatus()
		if err != nil || !strings.Contains(strings.Join(status.LastErrors, "\n"), local.ErrBusy.Error()) {
			t.Fatalf("initial census failure not recorded: %#v %v", status, err)
		}
		unlock()
		released = true
		return storagetest.NewMemoryStore(), nil
	}
	result, err := runOnePass(env, false)
	if err != nil || !opened || len(result.Errors) != 0 {
		t.Fatalf("later collector failed: %#v %v", result, err)
	}
	status, err := state.OpenReadOnly(home).LoadStatus()
	if err != nil || !strings.Contains(strings.Join(status.LastErrors, "\n"), local.ErrBusy.Error()) {
		t.Fatalf("preflight recovery failure erased: %#v %v", status.LastErrors, err)
	}
}
