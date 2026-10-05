package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
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
			for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
				records = append(records, json.RawMessage(line))
			}
			var first map[string]json.RawMessage
			if err = json.Unmarshal(records[0], &first); err != nil {
				t.Fatal(err)
			}
			var payload map[string]json.RawMessage
			json.Unmarshal(first["payload"], &payload)
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
