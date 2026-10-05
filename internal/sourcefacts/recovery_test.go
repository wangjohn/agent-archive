package sourcefacts

import (
	"context"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/archive"
	"path/filepath"
	"testing"
)

func TestRecoveryCompleteInventory(t *testing.T) {
	key := archive.RepoKey("git@example.test:acme/widget.git")
	for _, tc := range []struct {
		name     string
		projects []archive.ProjectActivation
		ids      map[string]RepositoryIdentity
		want     string
	}{
		{"unique", []archive.ProjectActivation{{Root: "/a", Included: true}}, map[string]RepositoryIdentity{"/a": {Root: "/a", Key: key, Known: true}}, ""},
		{"excluded clone", []archive.ProjectActivation{{Root: "/a", Included: true}, {Root: "/b", Included: false}}, map[string]RepositoryIdentity{"/a": {Root: "/a", Key: key, Known: true}, "/b": {Root: "/b", Key: key, Known: true}}, "project_ambiguous"},
		{"excluded unique", []archive.ProjectActivation{{Root: "/a", Included: false}}, map[string]RepositoryIdentity{"/a": {Root: "/a", Key: key, Known: true}}, "project_excluded"},
		{"unknown clone", []archive.ProjectActivation{{Root: "/a", Included: true}, {Root: "/b", Included: false}}, map[string]RepositoryIdentity{"/a": {Root: "/a", Key: key, Known: true}}, "project_inventory_unavailable"},
		{"scratch is known", []archive.ProjectActivation{{Root: "/a", Included: true}, {Root: "/scratch", Included: true}}, map[string]RepositoryIdentity{"/a": {Root: "/a", Key: key, Known: true}, "/scratch": {Known: true}}, ""},
		{"nested exclusion", []archive.ProjectActivation{{Root: "/a", Included: true}, {Root: "/a/private", Included: false}}, map[string]RepositoryIdentity{"/a": {Root: "/a", Key: key, Known: true}, "/a/private": {Root: "/a", Key: key, Known: true}}, "project_subtree_unavailable"},
		{"same basename different remote", []archive.ProjectActivation{{Root: "/a/widget", Included: true}}, map[string]RepositoryIdentity{"/a/widget": {Root: "/a/widget", Key: archive.RepoKey("https://other.test/org/widget"), Known: true}}, "project_repository_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			r := NewRecoveryResolver(tc.projects, nil, filepath.Clean, func(_ context.Context, root string) RepositoryIdentity { calls++; return tc.ids[root] }, nil)
			proof, outcome := r.Recover(context.Background(), "/gone/widget", key)
			if outcome != tc.want {
				t.Fatalf("%s: %+v", outcome, proof)
			}
			if outcome == "" && (proof.OriginalCwd != "/gone/widget" || proof.RecordedRepoKey != key || proof.Context == "" || proof.InventoryDigest == "") {
				t.Fatalf("missing proof %+v", proof)
			}
			r.Recover(context.Background(), "/gone/second", key)
			if calls != len(tc.projects) {
				t.Fatalf("inventory reread %d", calls)
			}
		})
	}
}

func TestRecoveryMappingsAndExplicitRules(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	projects := []archive.ProjectActivation{{Root: "/a", Included: true}, {Root: "/b", Included: false}, {Root: "/gone/private", Included: false}}
	calls := 0
	lookup := func(_ context.Context, path string) RepositoryIdentity {
		calls++
		return RepositoryIdentity{Root: path, Key: key, Known: true}
	}
	r := NewRecoveryResolver(projects, map[string]string{"/gone": "/a", "/gone/private": "/a"}, filepath.Clean, lookup, nil)
	proof, outcome := r.Recover(context.Background(), "/gone", key)
	if outcome != "" || proof.Root != "/a" || proof.Method != "explicit_mapping" {
		t.Fatalf("%+v %s", proof, outcome)
	}
	if calls != 1 {
		t.Fatalf("exact mapping unnecessarily swept other clones: %d", calls)
	}
	_, outcome = r.Recover(context.Background(), "/gone/private", key)
	if outcome != "project_excluded" {
		t.Fatal(outcome)
	}
	_, outcome = r.Recover(context.Background(), "/gone", archive.RepoKey("https://different.test/repo"))
	if outcome != "project_mapping_conflict" {
		t.Fatal(outcome)
	}
	_, outcome = r.Recover(context.Background(), "/gone/descendant", key)
	if outcome != "project_ambiguous" {
		t.Fatal(outcome)
	}
}

func TestRecoveryInventoryResumesAndCancellation(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	var projects []archive.ProjectActivation
	for i := 0; i < 300; i++ {
		projects = append(projects, archive.ProjectActivation{Root: fmt.Sprintf("/p%03d", i), Included: true})
	}
	inv := &RecoveryInventory{}
	calls := 0
	lookup := func(_ context.Context, path string) RepositoryIdentity {
		calls++
		k := ""
		if path == "/p299" {
			k = key
		}
		return RepositoryIdentity{Root: path, Key: k, Known: true}
	}
	for pass := 0; pass < 3; pass++ {
		r := NewRecoveryResolver(projects, nil, filepath.Clean, lookup, inv)
		_, outcome := r.Recover(context.Background(), "/gone", key)
		if pass < 2 && outcome != "project_budget_exhausted" {
			t.Fatal(outcome)
		}
		if pass == 2 && outcome != "" {
			t.Fatal(outcome)
		}
	}
	if calls != 300 {
		t.Fatalf("restarted inventory: %d", calls)
	}
	r := NewRecoveryResolver(projects, nil, filepath.Clean, lookup, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, outcome := r.Recover(ctx, "/gone", key)
	if outcome != "project_budget_exhausted" || r.Operations != 0 {
		t.Fatal(outcome, r.Operations)
	}
}

func BenchmarkRecoverySharedEvidence(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			projects := []archive.ProjectActivation{{Root: "/a", Included: true}}
			key := archive.RepoKey("https://example.test/acme/repo")
			calls := 0
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				r := NewRecoveryResolver(projects, nil, filepath.Clean, func(_ context.Context, path string) RepositoryIdentity {
					calls++
					return RepositoryIdentity{Root: path, Key: key, Known: true}
				}, nil)
				for j := 0; j < n; j++ {
					r.Recover(context.Background(), "/gone", key)
				}
			}
			b.ReportMetric(float64(calls)/float64(b.N), "lookups/op")
		})
	}
}
