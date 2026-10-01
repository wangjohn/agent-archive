package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// scopeDependencies is what finding a scope reads: the working directory,
// the lookup of a directory's repository key, and (through the data
// directory) the configured projects.
type scopeDependencies interface {
	workingDir() (string, error)
	repoKeyResolver() func(root string) string
	readHome() (string, error)
}

// sessionScope is the part of the archive a browser, `list`, or a title
// search looks at first: one repository, or one project by name. The zero
// value, and any scope with All set, holds every session.
type sessionScope struct {
	// Label names the scope for a person ("agent-archive"): the configured
	// project's folder, else the repository's main checkout (so a worktree
	// or a subdirectory reads alike), else the directory. Empty when the
	// working directory is in no project, which leaves nothing to narrow to.
	Label string
	// RepoKey is the repository key of Dir (archive.RepoKey of its origin
	// remote), which spans checkouts, worktrees, and machines. Empty with no
	// origin remote.
	RepoKey string
	// Dir is the directory the scope was made from; empty for a scope named
	// with --project NAME.
	Dir string
	// ProjectIDs are the archive project IDs Dir can belong to, or that the
	// configured projects named Label have.
	ProjectIDs []string
	// All is set when the scope is not applied: every session is in it.
	All bool
}

// scopeFor finds the scope of project, or of the working directory when
// project is empty: a directory, or a project's name (matched to
// project_name and the configured project labels, case-insensitively and
// exactly). The directory's repository key is looked up once. A directory in
// no project (no origin remote, and inside no configured project) has no
// scope (Label is empty and every session is in it), unless it was named with
// --project. allProjects keeps the
// scope's label and key but sets All, so a browser can still offer to narrow
// to it. The key comes from git under gitremote.Timeout, and is "" when git
// cannot answer in time. project and allProjects together are the caller's usage error.
func scopeFor(env scopeDependencies, project string, allProjects bool) (sessionScope, error) {
	home, err := env.readHome()
	if err != nil {
		return sessionScope{}, fmt.Errorf("resolve home: %w", err)
	}
	cfg, _, err := config.Load(home)
	if err != nil {
		return sessionScope{}, fmt.Errorf("load config: %w", err)
	}
	var dir string
	if project != "" {
		if !isDirectory(project) {
			return nameScope(cfg, project, allProjects), nil
		}
		abs, err := filepath.Abs(project)
		if err != nil {
			return sessionScope{}, fmt.Errorf("--project %s: %w", project, err)
		}
		dir = filepath.Clean(abs)
	} else {
		var ok bool
		if dir, ok = workingDirOrNone(env); !ok {
			return sessionScope{All: true}, nil
		}
	}
	key := env.repoKeyResolver()(dir)
	root, configured := configuredRoot(cfg, dir)
	// Only the working directory can be outside every project: a directory
	// named with --project is the scope, and may simply hold nothing.
	if key == "" && !configured && project == "" {
		return sessionScope{All: true}, nil
	}
	label := filepath.Base(dir)
	if configured {
		label = filepath.Base(root)
	} else if name, ok := repositoryName(dir); ok {
		label = name
	}
	ids := make([]string, 0, 4)
	for id := range archiveProjectIDs(cfg, dir) {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return sessionScope{Label: label, RepoKey: key, Dir: dir, ProjectIDs: ids, All: allProjects}, nil
}

// workingDirOrNone is the working directory, cleaned; ok is false when there
// is none (it was removed, say), which leaves no scope to find.
func workingDirOrNone(env workingDirDependencies) (string, bool) {
	dir, err := env.workingDir()
	if err != nil || dir == "" {
		return "", false
	}
	return filepath.Clean(dir), true
}

// isDirectory reports whether path is an existing directory: a --project
// value that is one names it, and any other is a project's name.
func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// repositoryName is the name of the git repository dir is in: the folder of
// its main checkout. A worktree's .git file points into the main checkout's
// .git/worktrees, so every checkout, worktree, and subdirectory of one
// repository is named alike, whatever sessions a command reads. A checkout
// reached through a symbolic link is named after the folder it links to, as
// git names it and its worktrees' .git files do. ok is false when dir is in
// no git checkout.
func repositoryName(dir string) (string, bool) {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		dotGit := filepath.Join(d, ".git")
		if info, err := os.Stat(dotGit); err == nil {
			if resolved, err := filepath.EvalSymlinks(d); err == nil {
				d, dotGit = resolved, filepath.Join(resolved, ".git")
			}
			if info.IsDir() {
				return filepath.Base(d), true
			}
			return linkedCheckoutName(d, dotGit), true
		}
		if filepath.Dir(d) == d {
			return "", false
		}
	}
}

// linkedCheckoutName names the checkout at root, whose .git is a file: a
// worktree's "gitdir: <main>/.git/worktrees/<name>" names it after <main>
// (or after a bare <main>.git, or the <main> holding a bare <main>/.bare);
// any other link (a submodule, a separate git directory) is its own
// repository, named after root.
func linkedCheckoutName(root, dotGit string) string {
	data, err := os.ReadFile(dotGit)
	if err != nil {
		return filepath.Base(root)
	}
	line, _, _ := strings.Cut(string(data), "\n")
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		return filepath.Base(root)
	}
	gitDir = strings.TrimSpace(gitDir)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(root, gitDir)
	}
	gitDir = filepath.Clean(gitDir)
	if filepath.Base(filepath.Dir(gitDir)) != "worktrees" {
		return filepath.Base(root)
	}
	common := filepath.Dir(filepath.Dir(gitDir))
	// A hidden git directory (.git, or the .bare of a bare repository kept
	// beside its worktrees) is named after the folder holding it.
	if strings.HasPrefix(filepath.Base(common), ".") {
		return filepath.Base(filepath.Dir(common))
	}
	if name := strings.TrimSuffix(filepath.Base(common), ".git"); name != "" {
		return name
	}
	return filepath.Base(root)
}

// nameScope is the scope of the projects called name: sessions whose project
// name is name, or that belong to a configured project labelled name.
func nameScope(cfg config.Config, name string, all bool) sessionScope {
	var ids []string
	for id, label := range projectLabels(cfg) {
		if strings.EqualFold(label, name) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return sessionScope{Label: name, ProjectIDs: ids, All: all}
}

// configuredRoot is the root of the deepest configured project holding dir.
func configuredRoot(cfg config.Config, dir string) (string, bool) {
	best := ""
	for _, p := range cfg.Archive.Projects {
		if p.Root != "" && sameProject(p.Root, dir) && len(p.Root) > len(best) {
			best = p.Root
		}
	}
	return best, best != ""
}

// archiveProjectIDs returns the archive project IDs dir can belong to: the
// configured project whose root contains it, and dir's own ID.
func archiveProjectIDs(cfg config.Config, dir string) map[string]bool {
	ids := map[string]bool{}
	for _, form := range pathForms(dir) {
		ids[archive.ProjectID(form)] = true
	}
	for _, project := range cfg.Archive.Projects {
		if project.Root != "" && sameProject(project.Root, dir) {
			ids[archive.ProjectID(project.Root)] = true
			if project.ProjectID != "" {
				ids[project.ProjectID] = true
			}
		}
	}
	return ids
}

// narrowed reports whether the scope cuts anything: it names a project and
// is not turned off.
func (s sessionScope) narrowed() bool { return s.Label != "" && !s.All }

// only returns the scope applied (All off), and everything the scope with
// All on.
func (s sessionScope) only() sessionScope {
	s.All = false
	return s
}

func (s sessionScope) everything() sessionScope {
	s.All = true
	return s
}

// contains reports whether a session is in the scope. m is its metadata
// (archived, or built from its transcript), and reg its registration on this
// machine, or nil. A session is in a repository's scope when its repository
// key is the scope's. With no key on either side (no origin remote, or an
// older session) it is in scope by path, as `--latest` decides: a local
// session when its project root contains the scope's directory, an archived
// one when its project ID is one the directory can have. A scope named with
// --project NAME holds the sessions of projects with that name.
func (s sessionScope) contains(m archive.Metadata, reg *archive.SessionRegistration) bool {
	if s.All || s.Label == "" {
		return true
	}
	key := m.RepoKey
	if reg != nil && reg.RepoKey != "" {
		key = reg.RepoKey
	}
	switch {
	case s.RepoKey != "" && key != "":
		return s.RepoKey == key
	case s.Dir == "":
		return s.hasName(m, reg)
	case reg != nil && reg.ProjectRoot != "":
		return sameProject(reg.ProjectRoot, s.Dir)
	}
	return m.ProjectID != "" && slices.Contains(s.ProjectIDs, m.ProjectID)
}

// hasName reports whether a session's project is the one the scope names.
func (s sessionScope) hasName(m archive.Metadata, reg *archive.SessionRegistration) bool {
	switch {
	case m.ProjectName != "" && strings.EqualFold(m.ProjectName, s.Label):
		return true
	case m.ProjectID != "" && slices.Contains(s.ProjectIDs, m.ProjectID):
		return true
	case reg != nil && reg.ProjectRoot != "":
		return strings.EqualFold(filepath.Base(filepath.Clean(reg.ProjectRoot)), s.Label)
	}
	return false
}

// filter keeps the sessions in the scope, in order.
func (s sessionScope) filter(sessions []archive.Metadata) []archive.Metadata {
	if s.All || s.Label == "" {
		return sessions
	}
	return slices.DeleteFunc(slices.Clone(sessions), func(m archive.Metadata) bool { return !s.contains(m, nil) })
}

// listScope is the `scope` object of `list --json`: what the listing looked
// at, so a script knows when the working directory narrowed it.
type listScope struct {
	// Label is the scope's name, even when the scope was not applied.
	Label string `json:"label"`
	// AllProjects is set when no scope was applied: --all-projects, or the
	// scope having nothing to list.
	AllProjects bool `json:"all_projects"`
	// FellBack is set when the scope held nothing, and the listing shows all
	// projects instead.
	FellBack bool `json:"fell_back"`
	// OutsideMatches is how many more sessions match the same filters outside
	// the scope; 0 when all projects are listed.
	OutsideMatches int `json:"outside_matches"`
}

// allProjectsLabel names the view with no scope.
const allProjectsLabel = "All projects"
