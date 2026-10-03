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
	anchors := map[string]string{}
	for _, project := range projects {
		if info, err := os.Stat(filepath.Join(project.Root, ".git")); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			child, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
			if key := env.projectRepoKey(child, project.Root); archive.IsRepoKey(key) {
				anchors[project.Root] = key
			}
			cancel()
		}
	}
	rules := make([]portableProjectRule, 0, len(projects))
	for _, project := range projects {
		rule := portableProjectRule{Path: homeRelative(project.Root, home), Included: project.Included}
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
				rule.RepoKey, rule.Path = anchors[anchor], filepath.ToSlash(rel)
			}
		}
		rules = append(rules, rule)
	}
	encoded, _ := json.Marshal(rules)
	return string(encoded)
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
	if err := decoder.Decode(new(any)); err != io.EOF {
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
			requests = append(requests, projectMatchRequest{RepoKey: rule.RepoKey})
		}
	}
	var matched projectMatchResult
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
		root := local.CanonicalPath(path)
		if anchor != "" && !local.PathWithin(root, anchor) {
			return []error{errors.New("--project-scope path resolves outside its repository checkout")}
		}
		if *rule.Included {
			var err error
			root, err = projectDir(path, home)
			if err != nil {
				return []error{fmt.Errorf("--project-scope: %w", err)}
			}
		}
		if seen[root] {
			return []error{errors.New("--project-scope contains duplicate paths")}
		}
		seen[root] = true
		resolved = append(resolved, archive.ProjectActivation{ProjectID: archive.ProjectID(root), Root: root, Included: *rule.Included})
	}
	for _, project := range resolved {
		updated := false
		for i := range cfg.Archive.Projects {
			if cfg.Archive.Projects[i].Root == project.Root {
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
