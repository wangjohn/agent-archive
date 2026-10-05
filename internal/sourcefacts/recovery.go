package sourcefacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

// RepositoryIdentity is a bounded local Git observation, supplied outside admission locks.
type RepositoryIdentity struct {
	Root         string
	Key          string
	Known        bool
	Dependencies []RepositoryDependency
}

// RepositoryDependency is a local metadata stamp, never Git config contents.
type RepositoryDependency struct {
	Path  string
	Stamp string
}
type RepositoryLookup func(context.Context, string) RepositoryIdentity

// RecoveryInventory carries an unfinished bounded configured-root sweep between discovery passes.
// Completed sweeps are refreshed by the next pass; they are never durable authority.
type RecoveryInventory struct {
	Context string               `json:"context"`
	Cursor  int                  `json:"cursor"`
	Entries []RepositoryIdentity `json:"entries"`
}

// RecoveryResolver only recovers absent checkouts into existing configured roots.
// Live ownership and explicit configured rules must be evaluated before calling Recover.
type RecoveryResolver struct {
	Projects      []archive.ProjectActivation
	Mappings      map[string]string
	ResolvePath   func(string) string
	Lookup        RepositoryLookup
	Validate      func(RepositoryIdentity) bool
	digest        string
	Inventory     *RecoveryInventory
	Context       string
	PolicyContext string
	Operations    int
	MaxOperations int
}

// NewRecoveryResolver freezes the policy context; no paths or URLs leave its digest.
func NewRecoveryResolver(projects []archive.ProjectActivation, mappings map[string]string, resolve func(string) string, lookup RepositoryLookup, inventory *RecoveryInventory) *RecoveryResolver {
	if inventory == nil {
		inventory = &RecoveryInventory{}
	}
	r := &RecoveryResolver{Projects: append([]archive.ProjectActivation(nil), projects...), Mappings: mappings, ResolvePath: resolve, Lookup: lookup, Inventory: inventory, MaxOperations: 128}
	sort.Slice(r.Projects, func(i, j int) bool { return r.Projects[i].Root < r.Projects[j].Root })
	r.Context = RecoveryContext(projects, mappings, filepath.Clean)
	r.PolicyContext = RecoveryContext(projects, nil, filepath.Clean)
	knownPaths := map[string]string{}
	for _, p := range projects {
		knownPaths[p.Root] = resolve(p.Root)
	}
	r.ResolvePath = func(path string) string {
		if canonical, ok := knownPaths[path]; ok {
			return canonical
		}
		return resolve(path)
	}
	if inventory.Context != r.Context || inventory.Cursor >= len(projects) || inventory.Cursor < 0 || len(inventory.Entries) != inventory.Cursor {
		*inventory = RecoveryInventory{Context: r.Context}
	}
	return r
}

// RecoveryContext binds proof and continuation to the complete configured inventory, including exclusions.
func RecoveryContext(projects []archive.ProjectActivation, mappings map[string]string, resolve func(string) string) string {
	type rule struct {
		Root      string
		Canonical string
		Included  bool
	}
	rules := make([]rule, 0, len(projects))
	for _, p := range projects {
		rules = append(rules, rule{p.Root, resolve(p.Root), p.Included})
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].Root < rules[j].Root })
	b, _ := json.Marshal(struct {
		Version  int
		Rules    []rule
		Mappings map[string]string
	}{1, rules, mappings})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Recover considers exact mappings and recorded keys without basename inference.
// Unknown inventory entries remain uncertainty, even when the included match looks unique.
func (r *RecoveryResolver) Recover(ctx context.Context, cwd, key string) (archive.ProjectResolution, string) {
	proof := archive.ProjectResolution{OriginalCwd: cwd, RecordedRepoKey: key, Context: r.Context, PolicyContext: r.PolicyContext}
	if len(cwd) > 4096 || !filepath.IsAbs(cwd) || strings.ContainsAny(cwd, "\x00\r\n") {
		return proof, "project_unavailable"
	}
	cwd = filepath.Clean(cwd)
	if p, ok := ConfiguredOwner(r.Projects, r.ResolvePath(cwd), r.ResolvePath); ok {
		if !p.Included {
			return proof, "project_excluded"
		}
		proof.Root = p.Root
		proof.Method = "configured_path"
		return proof, ""
	}
	target, mapped := r.Mappings[cwd]
	if mapped {
		for _, p := range r.Projects {
			if filepath.Clean(p.Root) != filepath.Clean(target) {
				continue
			}
			if !p.Included || r.Lookup == nil {
				return proof, "project_mapping_conflict"
			}
			if ctx.Err() != nil || r.Operations >= r.MaxOperations {
				return proof, "project_budget_exhausted"
			}
			id := r.Lookup(ctx, p.Root)
			r.Operations++
			if !id.Known {
				return proof, "project_inventory_unavailable"
			}
			if key != "" && id.Key != key {
				return proof, "project_mapping_conflict"
			}
			proof.Root = p.Root
			proof.Method = "explicit_mapping"
			return proof, ""
		}
		return proof, "project_mapping_conflict"
	}
	if !mapped && !archive.IsRepoKey(key) {
		return proof, "project_repository_unavailable"
	}
	if r.Lookup == nil || len(r.Projects) > 1024 {
		return proof, "project_inventory_unavailable"
	}
	inv := r.Inventory
	if r.Validate != nil {
		for i, id := range inv.Entries {
			if !r.Validate(id) {
				inv.Cursor = i
				inv.Entries = inv.Entries[:i]
				break
			}
		}
		r.Validate = nil
	}
	for inv.Cursor < len(r.Projects) {
		if ctx.Err() != nil || r.Operations >= r.MaxOperations {
			return proof, "project_budget_exhausted"
		}
		id := r.Lookup(ctx, r.Projects[inv.Cursor].Root)
		r.Operations++
		if ctx.Err() != nil {
			return proof, "project_budget_exhausted"
		}
		// Temporary unavailable answers must be retried, and cannot certify uniqueness.

		inv.Entries = append(inv.Entries, id)
		inv.Cursor++
	}
	if r.digest == "" {
		b, _ := json.Marshal(inv)
		sum := sha256.Sum256(b)
		r.digest = hex.EncodeToString(sum[:])
	}
	proof.InventoryDigest = r.digest
	for _, id := range inv.Entries {
		if !id.Known {
			return proof, "project_inventory_unavailable"
		}
	}
	matches := map[string]int{}
	for i, id := range inv.Entries {
		p := r.Projects[i]
		if id.Key != key || id.Root == "" {
			continue
		}
		root := r.ResolvePath(id.Root)
		// An inherited origin from a configured subtree cannot identify the missing subtree.
		if r.ResolvePath(p.Root) != root {
			return proof, "project_subtree_unavailable"
		}
		if old, ok := matches[root]; ok && r.Projects[old].Included != p.Included {
			return proof, "project_ambiguous"
		}
		matches[root] = i
	}
	if len(matches) != 1 {
		if len(matches) > 1 {
			return proof, "project_ambiguous"
		}
		return proof, "project_repository_unavailable"
	}
	for root, i := range matches {
		p := r.Projects[i]
		if !p.Included {
			return proof, "project_excluded"
		}
		for _, nested := range r.Projects {
			if !nested.Included && r.ResolvePath(nested.Root) != root && local.PathWithin(r.ResolvePath(nested.Root), root) {
				return proof, "project_subtree_unavailable"
			}
		}
		proof.Root = p.Root
		proof.Method = "recorded_repository"
		return proof, ""
	}
	return proof, "project_repository_unavailable"
}
