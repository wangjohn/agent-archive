package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// portableProjectRule keeps subtree decisions relative to a relocated checkout.
// Without a repository key, Path is absolute or relative to the user's home.
type portableProjectRule struct {
	RepoKey  string `json:"repo_key,omitempty"`
	Path     string `json:"path"`
	Included bool   `json:"included"`
}

func hasProjectExclusions(projects []archive.ProjectActivation) bool {
	for _, project := range projects {
		if !project.Included {
			return true
		}
	}
	return false
}

func portableProjectScope(projects []archive.ProjectActivation, home string, env Env, ctx context.Context) string {
	return portableProjectScopeWithKeys(projects, home, env, ctx, nil)
}

// portableProjectScopeWithKeys reuses only identities validated at their full
// canonical checkout root before switching the command's transport.
func portableProjectScopeWithKeys(projects []archive.ProjectActivation, home string, env Env, ctx context.Context, knownKeys map[string]string) string {
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
	roots := make(map[string]bool, len(projects))
	absolute := true
	for _, project := range projects {
		roots[project.Root] = true
		absolute = absolute && filepath.IsAbs(project.Root)
	}
	for _, project := range projects {
		// A checkout inside a path-based rule must move with that rule.
		// Relocating it alone would detach exclusions from their included
		// ancestor (or reinclusions from their excluded ancestor).
		if configuredProjectAncestor(project.Root, projects, roots, absolute) {
			continue
		}
		if key := knownKeys[project.Root]; archive.IsRepoKey(key) {
			anchors[project.Root] = key
			continue
		}
		if info, err := os.Stat(filepath.Join(project.Root, ".git")); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			child, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
			key, top, known := env.projectRepository(child, project.Root)
			if known && archive.IsRepoKey(key) && local.CanonicalPath(top) == project.Root {
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
		// Anchors are outermost configured roots, so at most one can own
		// this rule. Parent membership keeps work bounded by path depth.
		for root := project.Root; ; root = filepath.Dir(root) {
			if _, found := anchors[root]; found {
				anchor = root
				break
			}
			if filepath.Dir(root) == root {
				break
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

// configuredProjectAncestor tests strict ancestors of already canonical roots.
func configuredProjectAncestor(root string, projects []archive.ProjectActivation, roots map[string]bool, absolute bool) bool {
	if absolute {
		// For absolute roots, membership of a strict parent replaces pairwise
		// path cleaning and relative-path calculation between every sibling.
		for parent := filepath.Dir(root); parent != root; parent = filepath.Dir(parent) {
			if roots[parent] {
				return true
			}
			if filepath.Dir(parent) == parent {
				break
			}
		}
		return false
	}
	// Preserve PathWithin's behavior for unresolved relative roots.
	for _, other := range projects {
		if other.Root != root && local.PathWithin(root, other.Root) {
			return true
		}
	}
	return false
}

// hasProjectScope distinguishes an explicit empty flag from an absent flag.
func (o setupOptions) hasProjectScope() bool {
	return o.projectScopeSupplied || o.projectScopeFileSupplied || o.projectScope != "" || o.projectScopeFile != ""
}

// validateProjectScopeOptions refuses ambiguous scope before reading inputs or
// applying companion inclusions that could override transferred exclusions.
func validateProjectScopeOptions(o setupOptions) error {
	if o.projectScopeSupplied && strings.TrimSpace(o.projectScope) == "" {
		return errors.New("--project-scope must contain capture rules")
	}
	if o.projectScopeFileSupplied && strings.TrimSpace(o.projectScopeFile) == "" {
		return errors.New("--project-scope-file must name a file or - for stdin")
	}
	if (o.projectScopeSupplied || o.projectScope != "") && (o.projectScopeFileSupplied || o.projectScopeFile != "") && !o.projectScopeInputRead {
		return errors.New("give only one of --project-scope and --project-scope-file")
	}
	if o.hasProjectScope() && (len(o.projects) > 0 || len(o.projectRepos) > 0) {
		return errors.New("--project-scope and --project-scope-file cannot be combined with --project or --project-repo; include every capture rule in the scope")
	}
	return nil
}

// readProjectScopeInput reads only an explicitly selected file or stdin stream.
func readProjectScopeInput(opts setupOptions, stdin io.Reader) (setupOptions, error) {
	if err := validateProjectScopeOptions(opts); err != nil {
		return opts, err
	}
	if opts.projectScopeFile == "" {
		return opts, nil
	}
	if opts.projectScopeInputRead {
		return opts, nil
	}
	reader := stdin
	var file *os.File
	if opts.projectScopeFile != "-" {
		var err error
		file, err = os.Open(opts.projectScopeFile)
		if err != nil {
			return opts, errors.New("--project-scope-file cannot be opened")
		}
		reader = file
	}
	// 4096 native 4096-byte paths, each byte escaped as six JSON characters,
	// plus rule fields and repository keys fit below this bounded input size.
	const maxScopeBytes = 128 << 20
	data, err := io.ReadAll(io.LimitReader(reader, maxScopeBytes+1))
	if file != nil {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	if err != nil || len(data) > maxScopeBytes {
		return opts, errors.New("--project-scope-file is unreadable or exceeds 128 MiB")
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return opts, errors.New("--project-scope-file must contain capture rules")
	}
	opts.projectScope = string(data)
	opts.projectScopeInputRead = true
	return opts, nil
}

// setupProjectScope resolves every rule before changing the candidate config.
// A failed exclusion must never leave its ancestor newly included.
func setupProjectScope(cfg *config.Config, encoded, home string, env Env) []error {
	var rules []struct {
		RepoKey  string `json:"repo_key,omitempty"`
		Path     string `json:"path"`
		Included *bool  `json:"included"`
	}
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rules); err != nil {
		return []error{fmt.Errorf("--project-scope: %w", err)}
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return []error{errors.New("--project-scope must contain one JSON array")}
	}
	if len(rules) == 0 {
		return []error{errors.New("--project-scope must contain a nonempty JSON array")}
	}
	requests := []projectMatchRequest{}
	indices := map[string]int{}
	for _, rule := range rules {
		if rule.Path == "" || rule.Included == nil {
			return []error{errors.New("--project-scope requires a nonempty path and an included boolean for every rule")}
		}
		if rule.RepoKey == "" {
			continue
		}
		if !archive.IsRepoKey(rule.RepoKey) || !filepath.IsLocal(filepath.FromSlash(rule.Path)) {
			return []error{errors.New("--project-scope requires a valid repository key and a relative path within its checkout")}
		}
		if _, ok := indices[rule.RepoKey]; !ok {
			indices[rule.RepoKey] = len(requests)
			requests = append(requests, projectMatchRequest{RepoKey: rule.RepoKey, TransferredScope: true})
		}
	}
	matched := projectMatchResult{}
	if len(requests) > 0 {
		matched = matchProjects(context.Background(), env, home, *cfg, requests)
	}
	if len(requests) > 0 && matched.Incomplete {
		return []error{errors.New("--project-scope repository discovery is incomplete; retry before transferring capture rules")}
	}
	for key, index := range indices {
		if len(matched.Roots[index]) != 1 {
			return []error{fmt.Errorf("--project-scope repository %s needs exactly one eligible local clone; found %d", key, len(matched.Roots[index]))}
		}
	}
	resolved := make([]archive.ProjectActivation, 0, len(rules))
	seen := map[string]bool{}
	for _, rule := range rules {
		path := rule.Path
		anchor := ""
		if rule.RepoKey != "" {
			anchor = matched.Roots[indices[rule.RepoKey]][0]
			path = filepath.Join(anchor, filepath.FromSlash(path))
		} else if path == "~" || strings.HasPrefix(path, "~/") {
			path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
		} else if !filepath.IsAbs(path) {
			return []error{errors.New("--project-scope paths without a repository key must be absolute or start with ~/")}
		}
		root, err := resolveProjectScopePath(path)
		if err != nil {
			return []error{fmt.Errorf("--project-scope cannot resolve path safely: %w", err)}
		}
		if anchor != "" && !local.PathWithin(root, anchor) {
			return []error{errors.New("--project-scope path resolves outside its repository checkout")}
		}
		if *rule.Included {
			if _, err := projectDir(root, home); err != nil {
				return []error{fmt.Errorf("--project-scope: %w", err)}
			}
		}
		if seen[root] {
			return []error{errors.New("--project-scope contains duplicate paths")}
		}
		seen[root] = true
		resolved = append(resolved, archive.ProjectActivation{ProjectID: archive.ProjectID(root), Root: root, Included: *rule.Included})
	}
	return applyProjectScope(cfg, resolved)
}

// applyProjectScope checks destination consent before updating any capture rule.
func applyProjectScope(cfg *config.Config, resolved []archive.ProjectActivation) []error {
	saved := make([]archive.ProjectActivation, len(cfg.Archive.Projects))
	copy(saved, cfg.Archive.Projects)
	decisions := map[string]bool{}
	for i := range saved {
		saved[i].Root = local.CanonicalPath(saved[i].Root)
		if included, exists := decisions[saved[i].Root]; exists && included != saved[i].Included {
			return []error{errors.New("--project-scope saved aliases have conflicting capture decisions; review the saved capture scope first")}
		}
		decisions[saved[i].Root] = saved[i].Included
	}
	// Saved reinclusions must not defeat a transferred exclusion. Refuse
	// before writing anything, while keeping explicitly transferred reinclusions.
	for _, existing := range saved {
		root := existing.Root
		owner, found := nearestScopeRule(resolved, root)
		if existing.Included && found && !owner.Included && owner.Root != root {
			return []error{fmt.Errorf("--project-scope saved inclusion %s conflicts with transferred exclusion %s; review the saved capture scope first", existing.Root, owner.Root)}
		}
	}
	// An explicit transfer cannot silently override destination exclusions.
	for _, project := range resolved {
		if owner, found := nearestScopeRule(saved, project.Root); project.Included && found && !owner.Included {
			return []error{fmt.Errorf("--project-scope inclusion %s is blocked by saved exclusion %s; review the saved capture scope first", project.Root, owner.Root)}
		}
	}
	// Keep the original saved slice separate from candidate changes, including
	// when a caller passes a value copy of a config.
	cfg.Archive.Projects = append([]archive.ProjectActivation(nil), cfg.Archive.Projects...)
	for _, project := range resolved {
		updated := false
		for i, existing := range saved {
			if existing.Root == project.Root {
				cfg.Archive.Projects[i].Included = project.Included
				updated = true
			}
		}
		if !updated {
			cfg.Archive.Projects = append(cfg.Archive.Projects, project)
		}
	}
	return nil
}

// nearestScopeRule uses the same nearest-ancestor decision as capture, with
// paths canonicalized by the caller.
func nearestScopeRule(projects []archive.ProjectActivation, root string) (archive.ProjectActivation, bool) {
	var owner archive.ProjectActivation
	found := false
	for _, project := range projects {
		if local.PathWithin(root, project.Root) && (!found || len(project.Root) > len(owner.Root)) {
			owner, found = project, true
		}
	}
	return owner, found
}

// resolveProjectScopePath permits absent directories, but an existing symlink
// must resolve before its missing descendants can authorize a scope rule.
func resolveProjectScopePath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	ancestor := path
	for {
		if _, err = os.Lstat(ancestor); err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		next := filepath.Dir(ancestor)
		if next == ancestor {
			return "", err
		}
		ancestor = next
	}
	resolved, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(ancestor, path)
	if err != nil {
		return "", err
	}
	return local.CanonicalPath(filepath.Join(resolved, rel)), nil
}

// scopeArgumentsNeedStream selects transport before OS argv limits are reached.
func scopeArgumentsNeedStream(args []string) bool {
	bytes := 0
	for _, arg := range args {
		bytes += len(arg) + 1
	}
	return bytes > 64<<10
}
