package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestBackfillNativeChildOwnTaskPublicationWithoutParent(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(strconv.FormatBool(valid), func(t *testing.T) {
			canonical := func() string { p, e := filepath.EvalSymlinks(t.TempDir()); must(t, e); return p }
			home, userHome, project := canonical(), canonical(), canonical()
			native := filepath.Join(userHome, ".codex")
			must(t, os.MkdirAll(filepath.Join(native, "sessions"), 0700))
			at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
			const child = "11111111-1111-4111-8111-111111111111"
			const parent = "22222222-2222-4222-8222-222222222222"
			payload := map[string]any{"id": child, "session_id": parent, "parent_thread_id": parent, "cwd": project, "timestamp": at.Format(time.RFC3339Nano), "cli_version": "dev", "originator": "codex_cli_rs", "source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent, "depth": 1}}}, "subagent_history_start_ordinal": 1025}
			meta, e := json.Marshal(map[string]any{"type": "session_meta", "payload": payload})
			must(t, e)
			var raw strings.Builder
			raw.Write(meta)
			raw.WriteByte('\n')
			for range 1024 {
				raw.WriteString(`{"type":"turn_context","payload":{"model":"synthetic inherited","padding":"` + strings.Repeat("x", 512) + `"}}` + "\n")
			}
			task := child
			if !valid {
				task = "external-import-turn"
			}
			raw.Write(fmt.Appendf(nil, `{"type":"event_msg","timestamp":%q,"payload":{"type":"task_started","turn_id":%q,"started_at":%q}}`+"\n", at.Format(time.RFC3339Nano), task, at.Format(time.RFC3339Nano)))
			raw.WriteString(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic child own prompt"}]}}` + "\n")
			path := filepath.Join(native, "sessions", "rollout-"+child+".jsonl")
			must(t, os.WriteFile(path, []byte(raw.String()), 0600))
			cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, ImportedHarnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: at.Add(time.Hour)}}}}
			must(t, config.Save(home, cfg))
			cloud := storagetest.NewMemoryStore()
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), at.Add(2*time.Hour))
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return cloud, nil }
			beforePreview := snapshotTree(t, home)
			var preview, previewErr bytes.Buffer
			if code := Run([]string{"backfill", "--dry-run", "--harness", "codex", "--include-temp", "--project", project}, nil, &preview, &previewErr, env); code != 0 {
				t.Fatal("preview failed", code, previewErr.String())
			}
			if snapshotTree(t, home) != beforePreview {
				t.Fatal("native preview wrote local state")
			}
			var out, errOut bytes.Buffer
			code := Run([]string{"backfill", "--yes", "--background", "--harness", "codex", "--include-temp", "--project", project}, nil, &out, &errOut, env)
			if code != 0 {
				t.Fatalf("import failed %d %s %s", code, out.String(), errOut.String())
			}
			local, e := state.Open(home)
			must(t, e)
			regs, e := local.LoadRegistrations()
			must(t, e)
			if !valid {
				if len(regs) != 0 {
					t.Fatal("copied task licensed import", regs)
				}
				return
			}
			if len(regs) != 1 {
				t.Fatal("child not imported", out.String(), errOut.String(), regs)
			}
			reg := regs[0]
			if !reg.NativeChild || reg.NativeSessionID != child || reg.ParentSessionID != "" || reg.Origin != archive.SessionOriginImport || reg.ImportBatch.IsZero() || reg.CodexBinding == nil || reg.CodexBinding.OwnStart == nil || *reg.CodexBinding.OwnStart != 1025 || !reg.SessionStartedAt.Equal(at) {
				t.Fatal("independent provenance lost", reg)
			}
			published := false
			for range 8 {
				result, e := runOnePass(env, true)
				must(t, e)
				if len(result.Published) == 1 {
					published = true
					break
				}
			}
			if !published {
				t.Fatal("child publication did not converge")
			}
			objects, e := cloud.List(t.Context(), "")
			must(t, e)
			verified := false
			for _, object := range objects {
				if strings.HasSuffix(object.Key, "metadata.json") {
					metadata, e := reader.ReadMetadata(t.Context(), cloud, object.Key)
					must(t, e)
					bundle, e := reader.LoadSource(t.Context(), cloud, metadata, reader.Limits{})
					must(t, e)
					encoded, e := json.Marshal(bundle)
					must(t, e)
					if !strings.Contains(string(encoded), "Synthetic child own prompt") {
						t.Fatal("own prompt absent", string(encoded))
					}
					verified = true
				}
			}
			if !verified {
				t.Fatal("metadata not read back", objects)
			}
			assertNativeChildRecoveryRefused(t, local, reg, env)
			// A parent imported later may resolve the link, but cannot move the
			// independently admitted child into the parent's newer batch.
			parentAt := at.Add(-time.Hour)
			parentMeta, e := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": parent, "timestamp": parentAt.Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "codex_cli_rs", "cli_version": "dev"}})
			must(t, e)
			parentBody := fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q,"started_at":%q}}`+"\n"+`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic parent own prompt"}]}}`+"\n", parent, parentAt.Format(time.RFC3339Nano))
			must(t, os.WriteFile(filepath.Join(native, "sessions", "rollout-"+parent+".jsonl"), append(append(parentMeta, '\n'), []byte(parentBody)...), 0600))
			env.Now = func() time.Time { return at.Add(3 * time.Hour) }
			out.Reset()
			errOut.Reset()
			if code := Run([]string{"backfill", "--yes", "--background", "--harness", "codex", "--include-temp", "--project", project}, nil, &out, &errOut, env); code != 0 {
				t.Fatal("late parent import failed", code, errOut.String())
			}
			for range 8 {
				_, e := runOnePass(env, true)
				must(t, e)
			}
			linked, found, e := local.LoadRegistration(reg.ArchiveSessionID)
			must(t, e)
			if !found || linked.ParentSessionID == "" || linked.ImportBatch.Recorded() != reg.ImportBatch.Recorded() || linked.Origin != archive.SessionOriginImport {
				t.Fatal("late parent changed child membership", linked)
			}
			parentReg, found, e := local.LoadRegistration(linked.ParentSessionID)
			must(t, e)
			if !found || parentReg.NativeSessionID != parent || parentReg.ImportBatch.Recorded() == reg.ImportBatch.Recorded() {
				t.Fatal("late parent batch/provenance lost", parentReg)
			}
			assertNativeChildRecoveryRefused(t, local, linked, env)
			beforeRejected := bucketSnapshot(t, cloud)
			changed := strings.Replace(raw.String(), `"turn_id":"`+child+`"`, `"turn_id":"external-import-turn"`, 1)
			must(t, os.WriteFile(path, []byte(changed), 0600))
			rejected, e := runOnePass(env, true)
			must(t, e)
			if len(rejected.Published) != 0 || len(rejected.Errors) == 0 || bucketSnapshot(t, cloud) != beforeRejected {
				t.Fatal("ongoing copied task was published", rejected)
			}
			must(t, os.WriteFile(path, []byte(raw.String()), 0600))
			changedProject := filepath.Join(project, "different-worktree")
			must(t, os.Mkdir(changedProject, 0700))
			changedHeader := strings.Replace(raw.String(), `"cwd":"`+project+`"`, `"cwd":"`+changedProject+`"`, 1)
			must(t, os.WriteFile(path, []byte(changedHeader), 0600))
			changedResult, e := runOnePass(env, true)
			must(t, e)
			if len(changedResult.Published) != 0 || len(changedResult.Errors) == 0 || bucketSnapshot(t, cloud) != beforeRejected {
				t.Fatal("changed project source replaced admitted child", changedResult)
			}
			must(t, os.WriteFile(path, []byte(raw.String()), 0600))
			out.Reset()
			errOut.Reset()
			if code := Run([]string{"backfill", "undo", reg.ImportBatch.Recorded(), "--yes"}, nil, &out, &errOut, env); code != 0 {
				t.Fatal("actual child undo failed", code, errOut.String(), out.String())
			}
			reopened, e := state.Open(home)
			must(t, e)
			if regs, e := reopened.LoadRegistrations(); e != nil || len(regs) != 1 || regs[0].ArchiveSessionID != parentReg.ArchiveSessionID {
				t.Fatal("child undo touched external parent", regs, e)
			}
			removed, found, e := reopened.Removal("codex", child)
			must(t, e)
			if !found || removed.Reason != state.RemovalReasonUndo {
				t.Fatal("undo tombstone missing", removed)
			}
			out.Reset()
			errOut.Reset()
			if code := Run([]string{"backfill", "--yes", "--background", "--harness", "codex", "--include-temp", "--project", project}, nil, &out, &errOut, env); code != 0 {
				t.Fatal("repeated import failed", code, errOut.String())
			}
			if regs, e := reopened.LoadRegistrations(); e != nil || len(regs) != 1 || regs[0].ArchiveSessionID != parentReg.ArchiveSessionID {
				t.Fatal("child resurrected after undo", regs, e)
			}

		})
	}
}

// An unresolved native parent link does not make its child a top-level owner.
func assertNativeChildRecoveryRefused(t *testing.T, local *state.Store, reg archive.SessionRegistration, env Env) {
	t.Helper()
	published, err := local.LoadPublishedState(reg.ArchiveSessionID)
	must(t, err)
	bundle, at, found := published.LastPublished()
	if !found {
		t.Fatal("child recovery control requires an actual publication")
	}
	source, found := published.LastPublishedSource()
	if !found {
		t.Fatal("child recovery control requires its retained source reference")
	}
	metadata := published.Metadata()
	must(t, published.SaveBlocked(bundle, at, state.BlockedReasonTranscriptRewritten))
	before := snapshotTree(t, local.Home())
	for _, args := range [][]string{{"recover", reg.ArchiveSessionID}, {"recover", reg.ArchiveSessionID, "--confirm"}} {
		var out, errOut bytes.Buffer
		if code := Run(args, nil, &out, &errOut, env); code != 1 || !strings.Contains(errOut.String(), "top-level transcript files only") {
			t.Fatal("native child entered generation recovery", reg.ParentSessionID, code, out.String(), errOut.String())
		}
		if snapshotTree(t, local.Home()) != before {
			t.Fatal("refused native child recovery changed local authority")
		}
	}
	must(t, published.SavePublication(bundle, at, source, metadata))
}
