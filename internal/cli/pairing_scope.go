package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/gitremote"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/pairing"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

func portableHomePath(root, home string) string {
	rel, err := filepath.Rel(local.CanonicalPath(home), local.CanonicalPath(root))
	if err != nil || !pairing.RelativePath(filepath.ToSlash(rel)) {
		return ""
	}
	return filepath.ToSlash(rel)
}

func exportPairingScope(ctx context.Context, cfg config.Config, home string, env Env) ([]pairing.Inclusion, []pairing.Exclusion, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var inclusions []pairing.Inclusion
	var roots []string
	for _, project := range cfg.Archive.Projects {
		if !project.Included {
			continue
		}
		if len(inclusions) >= 128 {
			return nil, nil, fmt.Errorf("more than 128 scopes; reduce capture scope before pairing")
		}
		id, err := local.ID()
		if err != nil {
			return nil, nil, err
		}
		child, done := context.WithTimeout(ctx, 250*time.Millisecond)
		top, topErr := env.pairingRepositoryRoot(child, project.Root)
		done()
		root := local.CanonicalPath(project.Root)
		key, repoPath := "", ""
		if topErr == nil && filepath.IsAbs(top) && local.PathWithin(root, local.CanonicalPath(top)) {
			top = local.CanonicalPath(top)
			child, done = context.WithTimeout(ctx, 250*time.Millisecond)
			key = env.projectRepoKey(child, top)
			done()
			if key != "" {
				rel, e := filepath.Rel(top, root)
				if e == nil && pairing.RelativePath(filepath.ToSlash(rel)) {
					repoPath = filepath.ToSlash(rel)
				} else {
					key = ""
				}
			}
		}
		roots = append(roots, root)
		inclusions = append(inclusions, pairing.Inclusion{ID: id, RepoKey: key, RepoPath: repoPath, Label: filepath.Base(root), HomePath: portableHomePath(root, home)})
	}
	var exclusions []pairing.Exclusion
	for _, project := range cfg.Archive.Projects {
		if project.Included {
			continue
		}
		root := local.CanonicalPath(project.Root)
		var affected []string
		for i, inc := range inclusions {
			if local.PathWithin(root, roots[i]) {
				rel, _ := filepath.Rel(roots[i], root)
				exclusions = append(exclusions, pairing.Exclusion{InclusionID: inc.ID, Path: filepath.ToSlash(rel), Affected: []string{inc.ID}})
			} else if local.PathWithin(roots[i], root) {
				affected = append(affected, inc.ID)
			}
		}
		// An excluded ancestor excludes the entire inclusion even after relocation.
		for _, id := range affected {
			exclusions = append(exclusions, pairing.Exclusion{InclusionID: id, Path: ".", Affected: []string{id}})
		}
	}
	return inclusions, exclusions, nil
}

func resolvePortablePath(base, rel string) (string, error) {
	if !pairing.RelativePath(rel) {
		return "", fmt.Errorf("unsafe portable path")
	}
	base = local.CanonicalPath(base)
	root, err := local.ResolveExistingSymlinks(filepath.Join(base, filepath.FromSlash(rel)))
	if err != nil || !local.PathWithin(root, base) {
		return "", fmt.Errorf("portable path escapes its selected root")
	}
	return root, nil
}

func pairScope(p *prompter, payload pairing.Payload, existing config.Config, userHome string, env Env, yes bool) ([]archive.ProjectActivation, error) {
	matches := discoverPairingScope(payload, existing, userHome, env)
	selected, err := choosePairingScopes(p, payload, matches, userHome, yes)
	if err != nil {
		return nil, err
	}
	exclusions, withheld, err := mapPairingExclusions(p, payload, selected, userHome, yes)
	if err != nil {
		return nil, err
	}
	return appendPairingProjects(existing, payload.Inclusions, selected, exclusions, withheld, p, userHome), nil
}

func discoverPairingScope(payload pairing.Payload, existing config.Config, userHome string, env Env) projectMatchResult {
	requests := make([]projectMatchRequest, len(payload.Inclusions))
	for i, inc := range payload.Inclusions {
		requests[i].RepoKey = inc.RepoKey
		if inc.HomePath != "" {
			root, err := resolvePortablePath(userHome, inc.HomePath)
			if err == nil {
				if inc.RepoPath != "" && inc.RepoPath != "." {
					suffix := filepath.FromSlash(inc.RepoPath)
					if strings.HasSuffix(root, string(filepath.Separator)+suffix) {
						root = strings.TrimSuffix(root, string(filepath.Separator)+suffix)
					} else {
						root = ""
					}
				}
				requests[i].Path = root
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	matches := matchProjects(ctx, env, userHome, existing, requests)
	// Complete all bounded repository/path validation before waiting for consent.
	for i, inc := range payload.Inclusions {
		if inc.RepoPath == "" {
			continue
		}
		var validated []string
		for _, root := range matches.Roots[i] {
			child, done := context.WithTimeout(ctx, 250*time.Millisecond)
			top, e := env.pairingRepositoryRoot(child, root)
			done()
			if e != nil || local.CanonicalPath(top) != local.CanonicalPath(root) {
				continue
			}
			mapped, e := resolvePortablePath(root, inc.RepoPath)
			if e == nil {
				validated = append(validated, mapped)
			}
		}
		matches.Roots[i] = validated
	}
	return matches
}

func choosePairingScopes(p *prompter, payload pairing.Payload, matches projectMatchResult, userHome string, yes bool) (map[string][]string, error) {
	selected := map[string][]string{}
	for i, inc := range payload.Inclusions {
		roots := matches.Roots[i]
		if matches.Incomplete {
			roots = nil
		}
		if len(roots) > 1 {
			if yes {
				roots = nil
			} else {
				options := []option{{"skip", "Skip this repository"}}
				for j, root := range roots {
					options = append(options, option{fmt.Sprint(j + 1), homeRelative(root, userHome)})
				}
				options = append(options, option{"both", "Include every listed clone"})
				choice, err := p.menu("Choose a clone for "+inc.Label, "skip", options...)
				if err != nil {
					return nil, err
				}
				if choice == "both" {
					allow, err := p.yesNo("Include all listed clones?", false)
					if err != nil {
						return nil, err
					}
					if !allow {
						roots = nil
					}
				} else if choice == "skip" {
					roots = nil
				} else {
					for j := range roots {
						if choice == fmt.Sprint(j+1) {
							roots = []string{roots[j]}
							break
						}
					}
				}
			}
		}
		if len(roots) == 0 {
			terminal.Printf(p.out, "Skipped project %s: no unique complete eligible match; use setup --project to choose manually.\n", inc.Label)
		} else {
			selected[inc.ID] = append(selected[inc.ID], roots...)
		}
	}
	if matches.Incomplete {
		terminal.Printf(p.out, "Project discovery incomplete: timeout=%t cap=%t unreadable=%d.\n", matches.TimedOut, matches.Capped, matches.History.Unreadable)
	}
	return selected, nil
}

func mapPairingExclusions(p *prompter, payload pairing.Payload, selected map[string][]string, userHome string, yes bool) ([]archive.ProjectActivation, map[string]bool, error) {
	withheld := map[string]bool{}
	var exclusions []archive.ProjectActivation
	for _, exc := range payload.Exclusions {
		var mapped []string
		unresolved := exc.Unresolved
		if !unresolved {
			bases := selected[exc.InclusionID]
			if exc.HomeRelative {
				bases = []string{userHome}
			}
			for _, base := range bases {
				root, err := resolvePortablePath(base, exc.Path)
				if err != nil {
					unresolved = true
					break
				}
				mapped = append(mapped, root)
			}
			if len(bases) == 0 && !exc.HomeRelative {
				continue
			}
		}
		if exc.HomeRelative && !unresolved {
			for _, id := range exc.Affected {
				for _, root := range selected[id] {
					covered := false
					for _, exclusion := range mapped {
						covered = covered || local.PathWithin(root, exclusion)
					}
					if !covered {
						unresolved = true
					}
				}
			}
		}
		if unresolved {
			allow := false
			if !yes {
				var err error
				allow, err = p.yesNo("An exclusion could not be mapped. Explicitly include its affected projects without that source exclusion?", false)
				if err != nil {
					return nil, nil, err
				}
			}
			if !allow {
				for _, id := range exc.Affected {
					withheld[id] = true
				}
				terminal.Println(p.out, "Affected project scope withheld because an exclusion is unresolved.")
			}
			continue
		}
		for _, root := range mapped {
			exclusions = append(exclusions, archive.ProjectActivation{Root: root, ProjectID: archive.ProjectID(root), Included: false})
		}
	}
	return exclusions, withheld, nil
}

func appendPairingProjects(existing config.Config, inclusions []pairing.Inclusion, selected map[string][]string, exclusions []archive.ProjectActivation, withheld map[string]bool, p *prompter, userHome string) []archive.ProjectActivation {
	projects := slices.Clone(existing.Archive.Projects)
	for _, inc := range inclusions {
		if withheld[inc.ID] {
			continue
		}
		for _, root := range selected[inc.ID] {
			blocked := false
			for _, project := range existing.Archive.Projects {
				if !project.Included && (local.PathWithin(root, local.CanonicalPath(project.Root)) || local.PathWithin(local.CanonicalPath(project.Root), root)) {
					blocked = true
				}
			}
			if blocked {
				continue
			}
			present := false
			for _, project := range projects {
				if project.Root == root {
					present = true
				}
			}
			if !present {
				projects = append(projects, archive.ProjectActivation{Root: root, ProjectID: archive.ProjectID(root), Included: true})
			}
		}
	}
	for _, exc := range exclusions {
		present := false
		for _, project := range projects {
			if project.Root == exc.Root && !project.Included {
				present = true
			}
		}
		if !present {
			projects = append(projects, exc)
		}
	}

	// Stable output is also the exact scope reviewed before commit.
	for _, project := range projects {
		verb := "Exclude"
		if project.Included {
			verb = "Include"
		}
		terminal.Printf(p.out, "%s %s\n", verb, strings.TrimSpace(homeRelative(project.Root, userHome)))
	}
	return projects
}

func (e Env) pairingRepositoryRoot(ctx context.Context, root string) (string, error) {
	if e.PairingRepoRoot != nil {
		return e.PairingRepoRoot(ctx, root)
	}
	data, err := gitremote.ExecRunner(ctx, root, "-C", root, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	top := strings.TrimSpace(string(data))
	if !filepath.IsAbs(top) || strings.Contains(top, "\n") {
		return "", fmt.Errorf("repository root is unknown")
	}
	top = local.CanonicalPath(top)
	if !local.PathWithin(local.CanonicalPath(root), top) {
		return "", fmt.Errorf("repository root does not contain the inclusion")
	}
	return top, nil
}
