package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
)

func firstRunImportPlan(t *testing.T) (*backfillFixture, backfill.Plan, config.Config, *bool) {
	t.Helper()
	f := newBackfillFixture(t)
	// This transaction fixture reviews file evidence only. The general fixture's
	// deliberately unknown database ownership must not certify uniqueness.
	if err := os.Remove(macCursorDatabase(f.userHome)); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(f.data)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Archive.Projects = nil
	if err := config.Save(f.data, cfg); err != nil {
		t.Fatal(err)
	}
	const liveID = "0a9b3c4d-0000-4000-8000-0000000000aa"
	const goneID = "0a9b3c4d-0000-4000-8000-0000000000bb"
	relative := filepath.Join(".codex", "sessions", "2026", "09", "20", "rollout-2026-09-20T10-00-00-"+liveID+".jsonl")
	body, err := os.ReadFile(filepath.Join(f.userHome, relative))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(f.userHome, "agent-archive")
	gone := filepath.Join(f.userHome, ".codex", "worktrees", "deleted", "repo")
	copy := strings.ReplaceAll(string(body), liveID, goneID)
	copy = strings.Replace(copy, root, gone, 1)
	copy = strings.Replace(copy, `"source":"cli"`, `"source":"cli","git":{"repository_url":"https://example.test/acme/repo"}`, 1)
	f.write(t, strings.Replace(relative, liveID, goneID, 1), copy)
	env := f.env.backfillEnvironment(f.userHome, cfg)
	current := true
	env.RepositoryIdentity = func(_ context.Context, path string) sourcefacts.RepositoryIdentity {
		key := archive.RepoKey("https://example.test/acme/" + filepath.Base(path))
		if path == root {
			key = archive.RepoKey("https://example.test/acme/repo")
		}
		return sourcefacts.RepositoryIdentity{Root: path, Key: key, Known: true}
	}
	env.RepositoryIdentityCurrent = func(sourcefacts.RepositoryIdentity) bool { return current }
	p, err := backfill.BuildPlan(t.Context(), env, newArchiveState(f.data, cfg), cfg, backfill.Filters{Harnesses: []string{"codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Imported()) != 2 {
		t.Fatalf("%+v", p.Candidates)
	}
	return f, p, cfg, &current
}

func TestFirstRunImportConfirmationCommitsBeforeBackgroundAdmission(t *testing.T) {
	f, p, cfg, _ := firstRunImportPlan(t)
	var out, stderr bytes.Buffer
	code := importPlanLocked(f.env, &out, &stderr, f.data, p, configFingerprint(cfg), true)
	if code != 0 {
		t.Fatalf("%d %s", code, stderr.String())
	}
	confirmed, _, err := config.Load(f.data)
	if err != nil {
		t.Fatal(err)
	}
	if len(confirmed.Archive.Projects) != 1 {
		t.Fatalf("unexpected capture expansion: %+v", confirmed.Archive.Projects)
	}
	regs, err := state.OpenReadOnly(f.data).LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	imports := 0
	for _, reg := range regs {
		if reg.Imported() {
			imports++
		}
	}
	if imports != 2 {
		t.Fatalf("%d imported registrations", imports)
	}
}

func TestFirstRunImportConfirmationFailureDoesNotCommitProposedProjects(t *testing.T) {
	for _, failure := range []string{"stale", "cancelled", "batch_saved"} {
		t.Run(failure, func(t *testing.T) {
			f, p, cfg, current := firstRunImportPlan(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch failure {
			case "stale":
				*current = false
			case "cancelled":
				cancel()
			case "batch_saved":
				f.env.backfillCheckpoint = func(step string) error {
					if step == "batch saved" {
						return errors.New("synthetic config transition failure")
					}
					return nil
				}
			}
			if _, _, _, err := commitImport(ctx, f.env, f.data, p, configFingerprint(cfg)); err == nil {
				t.Fatal("confirmation unexpectedly succeeded")
			}
			after, _, err := config.Load(f.data)
			if err != nil {
				t.Fatal(err)
			}
			if len(after.Archive.Projects) != 0 {
				t.Fatal("failed confirmation committed capture projects")
			}
			regs, err := state.OpenReadOnly(f.data).LoadRegistrations()
			if err != nil {
				t.Fatal(err)
			}
			for _, reg := range regs {
				if reg.Imported() {
					t.Fatal("failed confirmation admitted session")
				}
			}
		})
	}
}
