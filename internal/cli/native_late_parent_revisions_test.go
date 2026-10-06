package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"os"
	"path/filepath"
	"reflect"
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

func TestNativeLateParentPreservedReadback(t *testing.T) {
	for _, during := range []bool{false, true} {
		name := "settled"
		if during {
			name = "preparing"
		}
		t.Run(name, func(t *testing.T) {
			origin := archive.SessionOriginHook
			canonical := func() string { p, err := filepath.EvalSymlinks(t.TempDir()); must(t, err); return p }
			home, userHome, project := canonical(), canonical(), canonical()
			nativeHome := filepath.Join(userHome, ".codex")
			must(t, os.MkdirAll(filepath.Join(nativeHome, "sessions"), 0700))
			at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
			const thread = "11111111-1111-4111-8111-111111111111"
			const physical = "22222222-2222-4222-8222-222222222222"
			const parentNative = "44444444-4444-4444-8444-444444444444"
			meta := func(base map[string]any, ordinal int) []byte {
				payload := map[string]any{"id": thread, "timestamp": at.Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "codex_cli_rs", "cli_version": "0.160.0", "history_mode": "paginated"}
				payload["parent_thread_id"] = parentNative
				payload["subagent_history_start_ordinal"] = 0
				if base != nil {
					payload["history_base"] = base
				}
				raw, err := json.Marshal(map[string]any{"type": "session_meta", "ordinal": ordinal, "timestamp": at.Format(time.RFC3339Nano), "payload": payload})
				must(t, err)
				return append(raw, '\n')
			}
			task := []byte(fmt.Sprintf(`{"type":"event_msg","ordinal":1,"timestamp":%q,"payload":{"type":"task_started","turn_id":"11111111-1111-4111-8111-111111111111","root_turn_id":"11111111-1111-4111-8111-111111111111","started_at":%q}}`+"\n", at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano)))
			seed := filepath.Join(nativeHome, "sessions", "rollout-2026-10-02T12-00-00-"+thread+".jsonl")
			prefix := append(meta(nil, 0), task...)
			must(t, os.WriteFile(seed, append(prefix, []byte(`{"type":"response_item","ordinal":2,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic outgoing-only"}]}}`+"\n")...), 0600))
			current := filepath.Join(nativeHome, "sessions", "rollout-2026-10-02T12-00-00-"+physical+".jsonl")
			raw := meta(map[string]any{"thread_id": thread, "end_ordinal_exclusive": 2, "end_byte_offset": len(prefix)}, 2)
			must(t, os.WriteFile(current, append(raw, []byte(`{"type":"response_item","ordinal":3,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic current-only"}]}}`+"\n")...), 0600))
			db, err := sql.Open("sqlite", filepath.Join(nativeHome, "state_5.sqlite"))
			must(t, err)
			defer func() { _ = db.Close() }()
			_, err = db.ExecContext(t.Context(), "CREATE TABLE threads(id TEXT PRIMARY KEY,rollout_path TEXT)")
			must(t, err)
			_, err = db.ExecContext(t.Context(), "INSERT INTO threads VALUES(?,?)", thread, current)
			must(t, err)
			cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, ImportedHarnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: at.Add(-time.Hour)}}}}
			must(t, config.Save(home, cfg))
			local, err := state.Open(home)
			must(t, err)
			reg := archive.SessionRegistration{ArchiveSessionID: "admitted-history", NativeSessionID: thread, Harness: archive.Harness{Name: "codex"}, ProjectID: archive.ProjectID(project), ProjectRoot: project, TranscriptPath: seed, SessionStartedAt: at, RegisteredAt: at.Add(time.Second), AdmittedAt: at.Add(time.Second), Origin: origin, NativeChild: true, ParentNativeSessionID: parentNative, NativeSourceHome: nativeHome, DestinationID: cfg.DestinationID()}
			must(t, local.SaveRegistration(reg))
			cloud := storagetest.NewMemoryStore()
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), at.Add(2*time.Minute))
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return cloud, nil }
			published := false
			var preparing state.PendingPublication
			for range 12 {
				result, err := runOnePass(env, true)
				must(t, err)
				for _, issue := range result.Errors {
					if !errors.Is(issue, archive.ErrHistoryMutationPending) && !agentapi.HasFailure(issue, agentapi.Unavailable) {
						t.Fatal(result.Errors)
					}
				}
				if during {
					p, found, err := local.LoadPending(reg.ArchiveSessionID)
					must(t, err)
					if found && p.History != nil && p.History.Preparing && p.History.PrivacyCursor > 0 {
						preparing = p
						published = true
						break
					}
				}
				if len(result.Published) > 0 {
					published = true
					break
				}
			}
			if !published {
				t.Fatal("admitted history did not publish with discovery disabled")
			}
			after, found, err := local.LoadRegistration(reg.ArchiveSessionID)
			must(t, err)
			if !found || after.CodexBinding == nil || after.CodexBinding.Home != nativeHome || after.Origin != origin || !after.AdmittedAt.Equal(reg.AdmittedAt) || after.NativeSessionID != thread {
				t.Fatal("trusted-home migration changed admission", after)
			}
			registrations, err := local.LoadRegistrations()
			must(t, err)
			if len(registrations) != 1 {
				t.Fatal("read-home migration created discovery owners")
			}
			var beforeBytes []byte
			if during {
				beforeBytes = preparing.MetadataBytes
			} else {
				beforeBytes, err = local.PublishedMetadata(reg.ArchiveSessionID)
				must(t, err)
			}
			var before archive.Metadata
			must(t, json.Unmarshal(beforeBytes, &before))
			originalSources := map[string]archive.SourceBundle{}
			if during {
				for _, input := range preparing.History.Inputs {
					raw, err := local.ReadPendingSource(reg.ArchiveSessionID, state.PendingSource{Reference: input.Reference, Name: input.Reference.SHA256 + ".gz"})
					must(t, err)
					originalSources[input.RevisionID], err = archive.ReadSourceBundle(bytes.NewReader(raw), archive.DecodeOptions{})
					must(t, err)
				}
			} else {
				originalSources[before.History.CurrentRevision], err = reader.LoadSource(t.Context(), cloud, before, reader.Limits{})
				must(t, err)
				for _, revision := range before.History.Preserved {
					originalSources[revision.RevisionID], err = reader.LoadRevision(t.Context(), cloud, before, revision.RevisionID, reader.Limits{})
					must(t, err)
				}
			}

			parentMeta, err := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": parentNative, "timestamp": at.Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "codex_cli_rs", "cli_version": "dev"}})
			must(t, err)
			parentTask := strings.ReplaceAll(string(task), thread, parentNative)
			parentPath := filepath.Join(nativeHome, "sessions", "rollout-"+parentNative+".jsonl")
			must(t, os.WriteFile(parentPath, append(append(parentMeta, '\n'), []byte(parentTask+`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic parent prompt"}]}}`+"\n")...), 0600))
			env.Now = func() time.Time { return at.Add(time.Hour) }
			var out, errOut bytes.Buffer
			if code := Run([]string{"backfill", "--yes", "--background", "--harness", "codex", "--include-temp", "--project", project}, nil, &out, &errOut, env); code != 0 {
				t.Fatal("late parent import failed", code, out.String(), errOut.String())
			}
			regs, err := local.LoadRegistrations()
			must(t, err)
			var parentReg archive.SessionRegistration
			for _, candidate := range regs {
				if candidate.NativeSessionID == parentNative {
					parentReg = candidate
				}
			}
			if parentReg.ArchiveSessionID == "" || parentReg.ImportBatch.IsZero() {
				t.Fatal("parent not independently imported")
			}
			for range 8 {
				result, err := runOnePass(env, true)
				must(t, err)
				for _, issue := range result.Errors {
					if !errors.Is(issue, archive.ErrHistoryMutationPending) {
						t.Fatal("late parent maintenance", result)
					}
				}
			}

			linked, found, err := local.LoadRegistration(reg.ArchiveSessionID)
			must(t, err)
			if !found || linked.ParentSessionID != parentReg.ArchiveSessionID {
				t.Fatal("late parent did not resolve", linked)
			}
			// This current segment reverts the original outgoing prompt. The
			// actual publication must retain that outgoing revision independently.
			if _, pending, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || pending {
				t.Fatal("newer link request was lost or never acknowledged", pending, err)
			}
			loads := state.PublishedStateLoads()
			for range 3 {
				result, err := runOnePass(env, true)
				must(t, err)
				if len(result.Errors) != 0 || len(result.Published) != 0 {
					t.Fatal("repaired child did not settle", result)
				}
			}
			if state.PublishedStateLoads() != loads {
				t.Fatal("settled child decoded published state")
			}
			key, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
			must(t, err)
			metadata, err := reader.ReadMetadata(t.Context(), cloud, key)
			must(t, err)
			if !metadata.CapturedAt.Equal(before.CapturedAt) || !reflect.DeepEqual(metadata.Counts, before.Counts) || !reflect.DeepEqual(metadata.ModelTokens, before.ModelTokens) {
				t.Fatal("parent repair changed child age or own activity")
			}
			for _, gap := range metadata.CaptureGaps {
				if gap.Code == "native_parent_link_pending" {
					t.Fatal("resolved parent remains pending")
				}
			}
			if metadata.ParentSessionID != parentReg.ArchiveSessionID {
				t.Fatal("parent repair not published", metadata.ParentSessionID)
			}
			currentBundle, err := reader.LoadSource(t.Context(), cloud, metadata, reader.Limits{})
			must(t, err)
			original := originalSources[metadata.History.CurrentRevision]
			if !reflect.DeepEqual(original.NativeRecords, currentBundle.NativeRecords) || !reflect.DeepEqual(original.Ordinals, currentBundle.Ordinals) || !reflect.DeepEqual(original.History.Spans, currentBundle.History.Spans) {
				t.Fatal("late parent repair changed active native evidence or raw ownership")
			}
			encoded, err := json.Marshal(currentBundle)
			must(t, err)
			if !strings.Contains(string(encoded), "synthetic current-only") || strings.Contains(string(encoded), "synthetic outgoing-only") || metadata.History == nil || len(metadata.History.Preserved) == 0 {
				t.Fatal("revert lost revision boundary/alternative", metadata.History)
			}
			must(t, os.Remove(seed))
			out.Reset()
			errOut.Reset()
			if code := Run([]string{"backfill", "undo", parentReg.ImportBatch.Recorded(), "--yes"}, nil, &out, &errOut, env); code != 0 {
				t.Fatal("parent undo failed", code, out.String(), errOut.String())
			}
			recovered := false
			for _, revision := range metadata.History.Preserved {
				previous, err := reader.LoadRevision(t.Context(), cloud, metadata, revision.RevisionID, reader.Limits{})
				must(t, err)
				original := originalSources[revision.RevisionID]
				if !previous.Capture.CapturedAt.Equal(original.Capture.CapturedAt) || !reflect.DeepEqual(original.NativeRecords, previous.NativeRecords) || !reflect.DeepEqual(original.Ordinals, previous.Ordinals) || !reflect.DeepEqual(original.History.Spans, previous.History.Spans) {
					t.Fatal("late parent repair changed preserved evidence, age or raw ownership")
				}
				data, err := json.Marshal(previous)
				must(t, err)
				recovered = recovered || strings.Contains(string(data), "synthetic outgoing-only")
			}
			if !recovered {
				t.Fatal("outgoing evidence unreadable after native dependency deletion")
			}
			// The preserved revisions keep their original ages and raw ownership.
			for _, old := range before.History.Preserved {
				for _, next := range metadata.History.Preserved {
					if old.RevisionID == next.RevisionID && !old.CapturedAt.Equal(next.CapturedAt) {
						t.Fatal("parent repair changed preserved capture age")
					}
				}
			}
			saved := mustLoadConfig(t, home)
			if saved.Discovery != nil && saved.Discovery.Enabled {
				t.Fatal("read-home migration enabled discovery permission")
			}
		})
	}
}
