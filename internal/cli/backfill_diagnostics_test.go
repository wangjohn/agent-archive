package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackfillDryRunExplainsDeletedWorktreeWithoutExposingSource(t *testing.T) {
	f := newBackfillFixture(t)
	const id = "0a9b3c4d-0000-4000-8000-0000000000aa"
	source := filepath.Join(f.userHome, ".codex", "sessions", "2026", "09", "20", "rollout-2026-09-20T10-00-00-"+id+".jsonl")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(f.userHome, ".codex", "worktrees", "deleted", "repo")
	data = []byte(strings.Replace(string(data), filepath.Join(f.userHome, "agent-archive"), gone, 1))
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, structured := range []bool{false, true} {
		args := []string{"--dry-run", "--harness", "codex"}
		if structured {
			args = append(args, "--json")
		}
		output, stderr, code := f.run(t, args...)
		if code != 0 {
			t.Fatalf("%d: %s", code, stderr)
		}
		for _, secret := range []string{id, source, gone, "inspect the file"} {
			if strings.Contains(output, secret) {
				t.Fatalf("output exposed %q", secret)
			}
		}
		if structured {
			var got struct {
				Skipped     map[string]int `json:"skipped"`
				Diagnostics []struct {
					Detail string `json:"detail"`
					Action string `json:"action"`
					Count  int    `json:"count"`
				} `json:"diagnostics"`
			}
			if err := json.Unmarshal([]byte(output), &got); err != nil {
				t.Fatal(err)
			}
			if got.Skipped["worktree_unresolved"] != 1 || len(got.Diagnostics) != 1 || got.Diagnostics[0].Detail != "project_repository_unavailable" || got.Diagnostics[0].Action != "review_project" || got.Diagnostics[0].Count != 1 {
				t.Fatalf("%+v", got)
			}
		} else if !strings.Contains(output, "Recorded repository identity is missing") {
			t.Fatal(output)
		}
	}
}
