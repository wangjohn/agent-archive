package backfill

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"path/filepath"
	"testing"
)

func TestDeletedWorktreeRecoveryEvidenceCache(t *testing.T) {
	tr := newTree(t)
	a, b := tr.repo("home/a"), tr.repo("home/b")
	gone := tr.path("home/.codex/worktrees/ab12/repo")
	ka, kb := archive.RepoKey("git@example.test:acme/a.git"), archive.RepoKey("https://example.test/acme/b")
	env := tr.env()
	calls := 0
	env.RepositoryIdentity = func(_ context.Context, root string) sourcefacts.RepositoryIdentity {
		calls++
		key := ka
		if root == b {
			key = kb
		}
		return sourcefacts.RepositoryIdentity{Root: root, Key: key, Known: true}
	}
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(a, true), project(b, true)}}}
	r := newResolver(env, cfg, Filters{})
	first, second := r.resolveEvidence(context.Background(), gone, ka), r.resolveEvidence(context.Background(), gone, kb)
	if first.root != a || second.root != b || first.proof == nil || second.proof == nil || calls != 2 {
		t.Fatalf("%+v %+v calls%d", first, second, calls)
	}
	if r.resolveEvidence(context.Background(), gone, ka).proof.OriginalCwd != gone {
		t.Fatal("lost cwd")
	}
	// A trustworthy configured live owner wins over recorded remote evidence.
	if live := r.resolveEvidence(context.Background(), a, kb); live.root != a {
		t.Fatal(live)
	}
}

func TestProjectMappingValidationAndBatchDigest(t *testing.T) {
	for _, bad := range []string{"relative=/a", "/a=relative", "/a=", "=/a"} {
		if _, err := ParseProjectMappings([]string{bad}); err == nil {
			t.Fatal(bad)
		}
	}
	if _, err := ParseProjectMappings([]string{"/a=/b", "/a=/c"}); err == nil {
		t.Fatal("conflicting mapping accepted")
	}
	maps, err := ParseProjectMappings([]string{"/old=/target=with=equals"})
	if err != nil || maps["/old"] != "/target=with=equals" {
		t.Fatal(maps, err)
	}
	tr := newTree(t)
	target := tr.repo("home/a")
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(target, true)}}}
	mappings := map[string]string{filepath.Join(tr.home, "gone"): target}
	if err := validateProjectMappings(tr.env(), cfg, mappings); err != nil {
		t.Fatal(err)
	}
	cfg.Archive.Projects[0].Included = false
	if err := validateProjectMappings(tr.env(), cfg, mappings); err == nil {
		t.Fatal("excluded target accepted")
	}
	a := Plan{Filters: Filters{ProjectMappings: mappings}}.BatchFilters()
	b := Plan{Filters: Filters{ProjectMappings: map[string]string{filepath.Join(tr.home, "gone"): "/other"}}}.BatchFilters()
	if a.equal(b) || a.MappingDigest == "" {
		t.Fatal("mapping change continued batch")
	}
}
