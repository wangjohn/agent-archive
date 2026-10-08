package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestRecoverRelatedHistoryUsesReadOnlyTrustedPreviewAndReopens(t *testing.T) {
	canonical := func() string { p, e := filepath.EvalSymlinks(t.TempDir()); must(t, e); return p }
	home, userHome, project := canonical(), canonical(), canonical()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	nativeHome := filepath.Join(userHome, ".codex")
	must(t, os.MkdirAll(filepath.Join(nativeHome, "sessions"), 0700))
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	const thread = "11111111-1111-4111-8111-111111111111"
	const physical = "22222222-2222-4222-8222-222222222222"
	meta := func(base map[string]any, ordinal int) []byte {
		payload := map[string]any{"id": thread, "timestamp": at.Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "codex_cli_rs", "cli_version": "0.160.0", "history_mode": "paginated"}
		if base != nil {
			payload["history_base"] = base
		}
		b, e := json.Marshal(map[string]any{"type": "session_meta", "ordinal": ordinal, "timestamp": at.Format(time.RFC3339Nano), "payload": payload})
		must(t, e)
		return append(b, '\n')
	}
	task := []byte(fmt.Sprintf(`{"type":"event_msg","ordinal":1,"timestamp":%q,"payload":{"type":"task_started","turn_id":"task","root_turn_id":"task","started_at":%q}}`+"\n", at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano)))
	seed := filepath.Join(nativeHome, "sessions", "rollout-2026-10-02T12-00-00-"+thread+".jsonl")
	prefix := append(meta(nil, 0), task...)
	must(t, os.WriteFile(seed, prefix, 0600))
	current := filepath.Join(nativeHome, "sessions", "rollout-2026-10-02T12-00-00-"+physical+".jsonl")
	raw := meta(map[string]any{"thread_id": thread, "end_ordinal_exclusive": 2, "end_byte_offset": len(prefix)}, 2)
	raw = append(raw, []byte(`{"type":"response_item","ordinal":3,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"current task"}]}}`+"\n")...)
	if os.Getenv("AGENT_ARCHIVE_RECOVER_SCALE") == "1" {
		for i := 4; i < 100004; i++ {
			raw = append(raw, []byte(fmt.Sprintf(`{"type":"response_item","ordinal":%d,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic recovery task %06d"}]}}`+"\n", i, i))...)
		}
	}
	must(t, os.WriteFile(current, raw, 0600))
	db, e := sql.Open("sqlite", filepath.Join(nativeHome, "state_5.sqlite"))
	must(t, e)
	defer func() { _ = db.Close() }()
	_, e = db.ExecContext(t.Context(), "CREATE TABLE threads(id TEXT PRIMARY KEY,rollout_path TEXT)")
	must(t, e)
	_, e = db.ExecContext(t.Context(), "INSERT INTO threads VALUES(?,?)", thread, current)
	must(t, e)
	cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: at.Add(-time.Hour)}}}}
	must(t, config.Save(home, cfg))
	local, e := state.Open(home)
	must(t, e)
	reg := archive.SessionRegistration{ArchiveSessionID: "admitted-history", NativeSessionID: thread, Harness: archive.Harness{Name: "codex"}, ProjectID: archive.ProjectID(project), ProjectRoot: project, TranscriptPath: seed, SessionStartedAt: at, RegisteredAt: at.Add(time.Second), AdmittedAt: at.Add(time.Second), Origin: archive.SessionOriginHook}
	must(t, local.SaveRegistration(reg))
	cloud := storagetest.NewMemoryStore()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at.Add(2*time.Minute))
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return cloud, nil }
	for range 5 {
		_, e = runOnePass(env, false)
		must(t, e)
	}
	p, e := local.LoadPublishedState(reg.ArchiveSessionID)
	must(t, e)
	b, publishedAt, found := p.LastPublished()
	if !found || b.History == nil {
		t.Fatal("history not published")
	}
	must(t, p.SaveBlocked(b, publishedAt, state.BlockedReasonTranscriptRewritten))
	catalogPath := filepath.Join(home, "discovery-catalog.json")
	catalogBefore, e := os.ReadFile(catalogPath)
	must(t, e)
	env.Now = func() time.Time { return at.Add(time.Hour) }
	var out, errOut bytes.Buffer
	code := Run([]string{"recover", reg.ArchiveSessionID}, nil, &out, &errOut, env)
	if code != 0 {
		t.Fatalf("actual related recovery preview failed: exit=%d error=%s output=%s", code, errOut.String(), out.String())
	}
	catalogAfter, e := os.ReadFile(catalogPath)
	must(t, e)
	if !bytes.Equal(catalogBefore, catalogAfter) {
		t.Fatal("preview changed capture catalog")
	}
	if _, found, e := local.GenerationSuccessor(reg.ArchiveSessionID); e != nil || found {
		t.Fatal("preview committed a successor", e)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"recover", reg.ArchiveSessionID, "--confirm"}, nil, &out, &errOut, env); code != 0 {
		t.Fatal("confirm", code, errOut.String())
	}
	local, e = state.Open(home)
	must(t, e)
	next, found, e := local.GenerationSuccessor(reg.ArchiveSessionID)
	must(t, e)
	if !found {
		t.Fatal("successor missing after reopen")
	}
	old, found, e := local.LoadRegistration(reg.ArchiveSessionID)
	must(t, e)
	if !found || !old.CaptureFrozen {
		t.Fatal("original was not frozen")
	}
	pending, found, e := local.LoadPending(next)
	must(t, e)
	if !found || pending.Bundle.History == nil || pending.Bundle.PreviousGenerationID != reg.ArchiveSessionID {
		t.Fatal("successor not self-contained")
	}
	var metadata archive.Metadata
	must(t, json.Unmarshal(pending.MetadataBytes, &metadata))
	decoded, e := reader.DecodeReferencedSource(t.Context(), metadata, pending.SourceBytes, reader.Limits{})
	must(t, e)
	if decoded.ArchiveSessionID != next || !decoded.Capture.CapturedAt.Equal(env.now()) || len(metadata.History.Preserved) != 0 {
		t.Fatal("successor identity/capture/reference set changed")
	}
	must(t, os.RemoveAll(nativeHome))
	result, e := runOnePass(env, false)
	must(t, e)
	if len(result.Errors) != 0 {
		t.Fatal("retained successor failed without native inputs", result.Errors)
	}
	if code := Run([]string{"recover", reg.ArchiveSessionID, "--confirm"}, nil, &out, &errOut, env); code != 0 {
		t.Fatal("repeated recovery", code, errOut.String())
	}
	again, found, e := local.GenerationSuccessor(reg.ArchiveSessionID)
	must(t, e)
	if !found || again != next {
		t.Fatal("repeated recovery changed identity")
	}
}
