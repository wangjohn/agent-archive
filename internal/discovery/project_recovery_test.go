package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeletedWorktreeDiscoveryRetainsCreationConsent(t *testing.T) {
	for _, recent := range []bool{false, true} {
		t.Run(map[bool]string{true: "new", false: "old"}[recent], func(t *testing.T) {
			store, cfg, at, root := fixture(t)
			gone := filepath.Join(t.TempDir(), "worktrees", "gone", "repo")
			start := at.Add(-time.Hour)
			if recent {
				start = at.Add(time.Minute)
			}
			id := writeRollout(t, root, gone, start, 1, "sessions")
			path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+id+".jsonl")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var records []json.RawMessage
			for line := range bytes.SplitSeq(bytes.TrimSpace(raw), []byte("\n")) {
				records = append(records, json.RawMessage(line))
			}
			var first map[string]json.RawMessage
			if err = json.Unmarshal(records[0], &first); err != nil {
				t.Fatal(err)
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(first["payload"], &payload); err != nil {
				t.Fatal(err)
			}
			payload["git"] = json.RawMessage(`{"repository_url":"git@example.test:acme/repo.git"}`)
			first["payload"], _ = json.Marshal(payload)
			records[0], _ = json.Marshal(first)
			var out []byte
			for _, record := range records {
				out = append(out, record...)
				out = append(out, '\n')
			}
			if err = os.WriteFile(path, out, 0600); err != nil {
				t.Fatal(err)
			}
			key := archive.RepoKey("https://example.test/acme/repo")
			calls := 0
			opts := Options{Now: func() time.Time { return at.Add(2 * time.Minute) }, RepositoryIdentity: func(_ context.Context, path string) sourcefacts.RepositoryIdentity {
				calls++
				return sourcefacts.RepositoryIdentity{Root: path, Key: key, Known: true}
			}}
			h, err := run(context.Background(), store, cfg, opts, syntheticSupport)
			want := 0
			if recent {
				want = 1
			}
			if err != nil || h.Registered != want {
				t.Fatalf("%+v %v", h, err)
			}
			regs, err := store.LoadRegistrations()
			if err != nil || len(regs) != want {
				t.Fatal(regs, err)
			}
			if recent && (regs[0].ProjectResolution == nil || regs[0].ProjectResolution.OriginalCwd != gone || regs[0].ProjectRoot != cfg.Archive.Projects[0].Root || regs[0].RepoKey != key) {
				t.Fatal(regs)
			}
			if recent {
				published, err := collector.Run(context.Background(), store, storagetest.NewMemoryStore(), collector.Options{Sources: builtin.NewBuiltins(), MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return at.Add(3 * time.Minute) }})
				if err != nil || len(published.Published) != 1 {
					t.Fatalf("recovered source publication/readback: %+v %v", published, err)
				}
			}
			wantCalls := 1
			if recent {
				wantCalls = 2
			}
			if calls != wantCalls {
				t.Fatalf("lookup count %d", calls)
			}
		})
	}
}

func TestDeletedWorktreeImportContinuesWithoutRemapping(t *testing.T) {
	store, cfg, at, root := fixture(t)
	gone := filepath.Join(t.TempDir(), "gone")
	native := writeRollout(t, root, gone, at.Add(-time.Hour), 7, "sessions")
	project := cfg.Archive.Projects[0].Root
	original, err := store.RegisterNewSession(agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: native}, func(id string) archive.SessionRegistration {
		return archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: native, Harness: archive.Harness{Name: "codex"}, ProjectID: archive.ProjectID(project), ProjectRoot: project, Origin: archive.SessionOriginImport, AdmittedAt: at.Add(time.Minute), SessionStartedAt: at.Add(-time.Hour), ProjectResolution: &archive.ProjectResolution{OriginalCwd: gone, Root: project, Method: "explicit_mapping", Context: "private-import-context"}}
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	opts := Options{Now: func() time.Time { return at.Add(3 * time.Minute) }, RepositoryIdentity: func(context.Context, string) sourcefacts.RepositoryIdentity {
		calls++
		return sourcefacts.RepositoryIdentity{}
	}}
	h, err := run(context.Background(), store, cfg, opts, syntheticSupport)
	if err != nil || h.Registered != 0 || h.Outcomes["admission_retry"] != 0 || calls != 0 {
		t.Fatalf("%+v %v calls%d", h, err, calls)
	}
	after, found, err := store.LoadRegistration(original.ArchiveSessionID)
	if err != nil || !found || after.ProjectRoot != project || after.Origin != archive.SessionOriginImport || !after.AdmittedAt.Equal(original.AdmittedAt) || after.ProjectResolution.Context != original.ProjectResolution.Context {
		t.Fatal(after, err)
	}
	if err := store.RecordRemoval("codex", native, state.RemovalReasonUndo, at.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	h, err = run(context.Background(), store, cfg, opts, syntheticSupport)
	if err != nil || h.Registered != 0 {
		t.Fatal(h, err)
	}
}

func TestProjectRecoveryMetadataBudgetStaysRetryable(t *testing.T) {
	resolver := sourcefacts.NewProjectResolver()
	resolver.Operations = 1024
	s := scan{resolver: resolver, recovery: sourcefacts.NewRecoveryResolver(nil, nil, filepath.Clean, nil, nil)}
	_, outcome, attempted := s.recoverProject(Candidate{WorkingDirectory: filepath.Join(t.TempDir(), "gone")})
	if !attempted || outcome != sourcefacts.RecoveryBudgetExhausted {
		t.Fatal(outcome, attempted)
	}
}
