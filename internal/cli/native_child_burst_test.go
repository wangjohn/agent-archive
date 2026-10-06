package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestNativeChildBurstKeepsFreshWorkMovingAcrossBoundedPasses(t *testing.T) {
	canonical := func() string { p, e := filepath.EvalSymlinks(t.TempDir()); must(t, e); return p }
	home, userHome, project := canonical(), canonical(), canonical()
	native := filepath.Join(userHome, ".codex")
	must(t, os.MkdirAll(filepath.Join(native, "sessions"), 0700))
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: at}}}, Discovery: &config.DiscoveryConfig{Enabled: true, CodexHomes: []string{native}}}
	must(t, config.ReconcileDiscovery(&cfg, config.Config{}, at))
	must(t, config.Save(home, cfg))
	db, e := sql.Open("sqlite", filepath.Join(native, "state_5.sqlite"))
	must(t, e)
	defer func() { must(t, db.Close()) }()
	_, e = db.ExecContext(t.Context(), "CREATE TABLE threads(id TEXT PRIMARY KEY,rollout_path TEXT)")
	must(t, e)
	write := func(n int) string {
		id := fmt.Sprintf("00000000-0000-0000-0000-%012d", n)
		parent := "ffffffff-ffff-4fff-8fff-ffffffffffff"
		header, e := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "timestamp": at.Add(time.Minute).Format(time.RFC3339Nano), "cwd": project, "originator": "codex_cli_rs", "cli_version": "dev", "source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent, "depth": 1}}}, "subagent_history_start_ordinal": 0}})
		must(t, e)
		body := fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q,"started_at":%q}}`+"\n"+`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic burst own prompt"}]}}`+"\n", id, at.Add(time.Minute).Format(time.RFC3339Nano))
		path := filepath.Join(native, "sessions", "rollout-"+id+".jsonl")
		must(t, os.WriteFile(path, append(append(header, '\n'), []byte(body)...), 0600))
		_, e = db.ExecContext(t.Context(), "INSERT INTO threads VALUES(?,?)", id, path)
		must(t, e)
		return id
	}
	const burst = 65
	for n := 1; n <= burst; n++ {
		write(n)
	}
	local, e := state.Open(home)
	must(t, e)
	cloud := storagetest.NewMemoryStore()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at.Add(2*time.Minute))
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return cloud, nil }
	first, e := runOnePass(env, true)
	must(t, e)
	health, found, e := discovery.ReadHealth(home)
	must(t, e)
	if !found || health.Probes > discovery.HeaderProbes {
		t.Fatal("burst did not make bounded progress", health, first)
	}
	// A fresh store has no absence proof yet. The real collector must finish
	// authoritative index recovery; discovery may not bypass that protection.
	if first.Scanned == 0 && health.Outcomes["admission_retry"] == 0 {
		t.Fatal("cold unknown index silently admitted or failed without pending", health)
	}
	fresh := write(66)
	freshPass := -1
	for pass := 1; pass <= 8; pass++ {
		_, e := runOnePass(env, true)
		must(t, e)
		health, found, e := discovery.ReadHealth(home)
		must(t, e)
		if !found || health.Probes > discovery.HeaderProbes {
			t.Fatal("pass probe limit exceeded", health)
		}
		regs, e := local.LoadRegistrations()
		must(t, e)
		for _, reg := range regs {
			if reg.NativeSessionID == fresh {
				// Background receipts have a separate five-readback cap. Assert
				// publication fairness directly through the real reader/store.
				metadataBytes, e := local.PublishedMetadata(reg.ArchiveSessionID)
				must(t, e)
				if len(metadataBytes) > 0 && freshPass < 0 {
					key, e := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
					must(t, e)
					metadata, e := reader.ReadMetadata(t.Context(), cloud, key)
					must(t, e)
					bundle, e := reader.LoadSource(t.Context(), cloud, metadata, reader.Limits{})
					must(t, e)
					encoded, e := json.Marshal(bundle)
					must(t, e)
					if bundle.NativeSessionID != fresh || !bundle.NativeChild || bundle.ProjectID != archive.ProjectID(project) || !strings.Contains(string(encoded), "Synthetic burst own prompt") {
						t.Fatal("fresh remote source identity/own context mismatch")
					}
					if _, pending, e := local.LoadRequest(reg.ArchiveSessionID); e != nil || pending {
						t.Fatal("fresh published child retains pending capture request", pending, e)
					}
					freshPass = pass
				}
			}
		}
		if len(regs) == burst+1 && freshPass > 0 {
			break
		}
	}
	regs, e := local.LoadRegistrations()
	must(t, e)
	if len(regs) != burst+1 || freshPass < 1 {

		t.Fatal("burst pinned fresh child or failed to converge", len(regs), freshPass)
	}
	for _, reg := range regs {
		if !reg.NativeChild || reg.Origin != archive.SessionOriginDiscovery || !reg.ImportBatch.IsZero() || !reg.HookObservedAt.IsZero() {
			t.Fatal("burst invented import/stop provenance", reg)
		}
	}
	t.Logf("actual burst: %d existing children plus fresh child; fresh verified on followup pass%d; native parent absent", burst, freshPass)
}
