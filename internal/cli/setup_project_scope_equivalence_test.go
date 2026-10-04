package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

// referencePortableProjectScope retains the original ancestor algorithm and current
// cached repository-key contract as an output and Git-call oracle.
func referencePortableProjectScope(projects []archive.ProjectActivation, home string, env Env, ctx context.Context, cachedKeys map[string]string) string {
	// Capture compares resolved locations, so aliases must share an anchor.
	canonical := make([]archive.ProjectActivation, 0, len(projects))
	seen := map[string]bool{}
	for _, project := range projects {
		project.Root = local.CanonicalPath(project.Root)
		// Equal decisions coalesce. Conflicts remain duplicate rules so the
		// receiver refuses atomically; latched capture identities can differ
		// from today's aliases, so choosing an inclusion could widen scope.
		if included, exists := seen[project.Root]; !exists || included != project.Included {
			seen[project.Root] = project.Included
			canonical = append(canonical, project)
		}
	}
	projects = canonical
	home = local.CanonicalPath(home)
	anchors := map[string]string{}
	for _, project := range projects {
		// A checkout inside a path-based rule must move with that rule.
		// Relocating it alone would detach exclusions from their included
		// ancestor (or reinclusions from their excluded ancestor).
		hasAncestor := false
		for _, other := range projects {
			if other.Root != project.Root && local.PathWithin(project.Root, other.Root) {
				hasAncestor = true
				break
			}
		}
		if hasAncestor {
			continue
		}
		if info, err := os.Stat(filepath.Join(project.Root, ".git")); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			child, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
			key, checked := cachedKeys[project.Root]
			top, known := project.Root, checked
			if !checked {
				key, top, known = env.projectRepository(child, project.Root)
			}
			if known && archive.IsRepoKey(key) && local.CanonicalPath(top) == local.CanonicalPath(project.Root) {
				anchors[project.Root] = key
			}
			cancel()
		}
	}
	// Distinct configured clones cannot share one relocation identity: their
	// root rules would collapse to the same path and their exclusions may differ.
	counts := map[string]int{}
	for _, key := range anchors {
		counts[key]++
	}
	for root, key := range anchors {
		if counts[key] > 1 {
			delete(anchors, root)
		}
	}
	rules := make([]portableProjectRule, 0, len(projects))
	for _, project := range projects {
		path, repoKey := homeRelative(project.Root, home), ""
		// Keep nested checkouts under the outer scope: relocating an excluded
		// nested repo independently would leave its old subtree included.
		anchor := ""
		for root := range anchors {
			if local.PathWithin(project.Root, root) && (anchor == "" || len(root) < len(anchor)) {
				anchor = root
			}
		}
		if anchor != "" {
			if rel, err := filepath.Rel(anchor, project.Root); err == nil {
				repoKey, path = anchors[anchor], filepath.ToSlash(rel)
			}
		}
		rules = append(rules, portableProjectRule{RepoKey: repoKey, Path: path, Included: project.Included})
	}
	encoded, _ := json.Marshal(rules)
	return string(encoded)
}

func TestPortableScopeAncestorLookupPreservesOutput(t *testing.T) {
	home := t.TempDir()
	outer := filepath.Join(home, "outer")
	nested := filepath.Join(outer, "nested")
	sibling := filepath.Join(home, "outer-collision")
	for _, root := range []string{outer, nested, sibling} {
		if err := os.MkdirAll(filepath.Join(root, ".git"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(home, "alias")
	if err := os.Symlink(outer, alias); err != nil {
		t.Fatal(err)
	}
	loop := filepath.Join(home, "loop")
	if err := os.Symlink("loop", loop); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relativeLoop, err := filepath.Rel(cwd, loop)
	if err != nil {
		t.Fatal(err)
	}
	cases := [][]archive.ProjectActivation{
		{{Root: outer, Included: true}, {Root: nested, Included: false}, {Root: sibling, Included: true}},
		{{Root: nested, Included: true}, {Root: outer, Included: false}, {Root: outer, Included: true}},
		{{Root: alias, Included: true}, {Root: outer, Included: false}, {Root: filepath.Join(alias, "nested"), Included: true}},
		{{Root: string(filepath.Separator), Included: false}, {Root: outer + string(filepath.Separator), Included: true}},
		{{Root: relativeLoop, Included: false}, {Root: outer, Included: true}},
		{{Root: ".", Included: true}, {Root: "relative/missing", Included: false}, {Root: outer, Included: true}},
	}
	for i, projects := range cases {
		for _, sharedKey := range []bool{false, true} {
			for cacheCase, cachedKeys := range []map[string]string{
				nil,
				{outer: archive.RepoKey("https://example.test/cached.git")},
				{outer: "", sibling: "invalid-key"},
				{outer: archive.RepoKey("https://example.test/shared.git"), sibling: archive.RepoKey("https://example.test/shared.git")},
			} {
				var calls []string
				env := Env{repoKeyContext: func(_ context.Context, root string) string {
					calls = append(calls, root)
					if sharedKey {
						return archive.RepoKey("https://example.test/shared.git")
					}
					return archive.RepoKey("https://example.test/" + filepath.Base(root) + ".git")
				}}
				want := referencePortableProjectScope(projects, home, env, t.Context(), cachedKeys)
				wantCalls := append([]string(nil), calls...)
				calls = nil
				got := portableProjectScope(projects, home, env, t.Context(), cachedKeys)
				if got != want || !reflect.DeepEqual(calls, wantCalls) {
					t.Fatalf("case %d shared=%v cache=%d output or repository calls changed: got %s calls %v want %s calls %v", i, sharedKey, cacheCase, got, calls, want, wantCalls)
				}
			}
		}
	}
}

func TestPortableScopeLargeSiblingOutputKeepsEveryRuleInOrder(t *testing.T) {
	home := t.TempDir()
	long := strings.Repeat(strings.Repeat("x", 240)+"/", 4)
	projects := make([]archive.ProjectActivation, 0, 4096)
	want := make([]portableProjectRule, 0, 4096)
	for i := range 4096 {
		path := filepath.Join(long, fmt.Sprintf("rule-%04d", i))
		projects = append(projects, archive.ProjectActivation{Root: filepath.Join(home, path), Included: i == 0})
		// These absent sibling directories have no ancestors in the configured
		// set and no repository metadata, so the original serialized rule is
		// the home-relative path with its original inclusion and order.
		want = append(want, portableProjectRule{Path: "~/" + filepath.ToSlash(path), Included: i == 0})
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if got := portableProjectScope(projects, home, Env{}, t.Context(), nil); got != string(encoded) {
		t.Fatal("large sibling scope changed paths, decisions, order, or serialization")
	}
}
