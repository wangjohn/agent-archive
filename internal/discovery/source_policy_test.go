package discovery

import (
	"github.com/wangjohn/agent-archive/internal/state"

	"github.com/wangjohn/agent-archive/internal/testutil/recoverytest"

	"context"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/local"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScannerRejectsImportedFirstTaskDespiteLaterNativeResume(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	native := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 1, "sessions")
	path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+native+".jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	imported, _ := json.Marshal(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": "external-import-turn-1", "root_turn_id": nil}})
	raw = []byte(lines[0] + "\n" + string(imported) + "\n" + strings.Join(lines[1:], "\n"))
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	h, err := run(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport)
	if err != nil || h.Registered != 0 || h.Outcomes["inherited_history"] == 0 {
		t.Fatalf("imported first turn admitted: %#v %v", h, err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatalf("import allocated: %v %v", regs, err)
	}
}

func TestScannerAcceptsPaginatedNativeWorktreeForConfiguredMain(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	project := cfg.Archive.Projects[0].Root
	checkout, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitdir := filepath.Join(project, ".git", "worktrees", "synthetic")
	if err := os.MkdirAll(gitdir, 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{filepath.Join(checkout, ".git"): "gitdir: " + gitdir + "\n", filepath.Join(gitdir, "commondir"): "../..\n", filepath.Join(gitdir, "gitdir"): filepath.Join(checkout, ".git") + "\n"}
	for path, data := range files {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	native := writeRollout(t, root, checkout, at.Add(time.Minute), 1, "sessions")
	path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+native+".jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	first["payload"].(map[string]any)["history_mode"] = "paginated"
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	lines[0] = string(encoded)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		t.Fatal(err)
	}
	h, err := run(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport)
	if err != nil || h.Registered != 1 {
		t.Fatalf("paginated worktree rejected: %#v %v", h, err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 1 || regs[0].ProjectRoot != project || regs[0].DiscoveryCwd != checkout {
		t.Fatalf("worktree attribution=%v error=%v", regs, err)
	}
}

func TestIncompleteSourceRetriesWhenFirstTaskArrives(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	native := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 1, "sessions")
	path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+native+".jsonl")
	complete, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(complete), "\n")
	if err := os.WriteFile(path, []byte(lines[0]+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	options := Options{Now: func() time.Time { return at.Add(2 * time.Minute) }}
	h, err := run(context.Background(), store, cfg, options, syntheticSupport)
	if err != nil || h.Registered != 0 || h.Outcomes["incomplete_metadata"] == 0 {
		t.Fatalf("partial task admitted: %#v %v", h, err)
	}
	if err := os.WriteFile(path, complete, 0600); err != nil {
		t.Fatal(err)
	}
	h, err = run(context.Background(), store, cfg, options, syntheticSupport)
	if err != nil || h.Registered != 1 {
		t.Fatalf("completed task stayed pending: %#v %v", h, err)
	}
}

func TestPreviousCatalogCannotBypassRevisedNegativeClassifications(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	native := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 1, "sessions")
	name := "rollout-2026-10-01T12-00-00-" + native + ".jsonl"
	adapter := codexAdapter{supported: syntheticSupport}
	entry := adapter.Describe(root, "sessions", name)
	prior := adapter.Inspect(context.Background(), entry.Source)
	if prior.Outcome != outcomeUsable {
		t.Fatal("fixture not usable")
	}
	raw, err := os.ReadFile(entry.Source.Locator)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	first["payload"].(map[string]any)["thread_source"] = "guardian_review"
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	lines[0] = string(encoded)
	if err := os.WriteFile(entry.Source.Locator, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		t.Fatal(err)
	}
	entry = adapter.Describe(root, "sessions", name)
	now := at.Add(2 * time.Minute)
	// Establish census-validated absence so an older cached usable observation
	// would allocate immediately without being reread after a recovery request.
	unlock, err := local.NamedLock(store.Home(), "hooks.lock")
	if err != nil {
		t.Fatal(err)
	}
	err = store.RequestSessionIndexRecovery(sessionKey("codex", native))
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := recoverytest.Exhaust(context.Background(), store, state.SessionIndexRecoverySlice, false); err != nil {
		t.Fatal(err)
	}
	// Version1 did not include supplementary classification fields in cached
	// candidates. Recreate its usable result with the current file fingerprint.
	old := catalog{Version: 1, Roots: []string{root}, Cache: map[string]cached{entry.Source.Locator: {Size: entry.Fingerprint.Size, Mtime: entry.Fingerprint.Mtime, Checked: now, Observation: prior}}}
	if err := local.Write(filepath.Join(store.Home(), "discovery-catalog.json"), old); err != nil {
		t.Fatal(err)
	}
	h, err := run(context.Background(), store, cfg, Options{Now: func() time.Time { return now }}, syntheticSupport)
	if err != nil || h.Registered != 0 || h.Outcomes["unsupported_execution"] == 0 {
		t.Fatalf("old cache bypassed classifier: %#v %v", h, err)
	}
}
