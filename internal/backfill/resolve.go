package backfill

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/wangjohn/agent-archive/internal/config"
)

// resolution is where one working directory is imported, or why it is not.
type resolution struct {
	// root is the project root, in the spelling it is registered under. It is
	// also set for the skips that have a would-be root (excluded, home,
	// temporary), so --project can match them.
	root string
	kind ProjectKind
	// included is set when a configured, included project owns the directory.
	included bool
	skip     SkipReason
}

// resolver applies the spec's project resolution rules. It never runs git:
// worktrees are followed through their .git files.
type resolver struct {
	env     Environment
	cfg     config.Config
	filters Filters
	// home and homeRaw are the home directory resolved and as given.
	home, homeRaw string
	// scratch holds the Claude desktop scratch-workspaces folder, resolved
	// and as given.
	scratch []string
	temps   []string
	// worktreeStores are the folders Codex and Cursor keep their worktrees
	// in; a missing worktree there cannot be mapped to its repository.
	worktreeStores []string
	cache          map[string]resolution
}

func newResolver(env Environment, cfg config.Config, filters Filters) *resolver {
	r := &resolver{env: env, cfg: cfg, filters: filters, homeRaw: filepath.Clean(env.Home), cache: map[string]resolution{}}
	r.home = env.resolved(env.Home)
	scratch := filepath.Join(env.Home, "Library", "Application Support", "Claude", "scratch-workspaces")
	r.scratch = uniquePaths(filepath.Clean(scratch), env.resolved(scratch))
	for _, t := range env.tempDirs() {
		if t != "" {
			r.temps = append(r.temps, uniquePaths(filepath.Clean(t), env.resolved(t))...)
		}
	}
	for _, store := range []string{filepath.Join(env.Home, ".codex", "worktrees"), filepath.Join(env.Home, ".cursor", "worktrees")} {
		r.worktreeStores = append(r.worktreeStores, uniquePaths(filepath.Clean(store), env.resolved(store))...)
	}
	return r
}

func uniquePaths(paths ...string) []string {
	var out []string
	for _, p := range paths {
		dup := false
		for _, o := range out {
			dup = dup || o == p
		}
		if !dup {
			out = append(out, p)
		}
	}
	return out
}

// pathWithin reports whether path is root or lies beneath it, lexically.
func pathWithin(path, root string) bool {
	if path == root {
		return true
	}
	if !strings.HasSuffix(root, string(filepath.Separator)) {
		root += string(filepath.Separator)
	}
	return strings.HasPrefix(path, root)
}

func withinAny(path string, roots []string) bool {
	for _, root := range roots {
		if pathWithin(path, root) {
			return true
		}
	}
	return false
}

// resolve maps a session's working directory to a project, applying the
// spec's rules in order; the first that matches wins.
func (r *resolver) resolve(cwd string) resolution {
	if cached, ok := r.cache[cwd]; ok {
		return cached
	}
	res := r.resolveUncached(cwd)
	r.cache[cwd] = res
	return res
}

func (r *resolver) resolveUncached(cwd string) resolution {
	// Rule 1: the working directory. Without one there is no project.
	if cwd == "" || !filepath.IsAbs(cwd) {
		return resolution{skip: SkipProjectUnknown}
	}
	dir := r.env.resolved(cwd)

	// Rule 2: the nearest configured ancestor, on resolved paths, as hooks
	// decide. An exclusion always beats the defaults below.
	if project, ok := r.configuredOwner(dir); ok {
		if !project.Included {
			return resolution{root: project.Root, kind: r.kindOf(project.Root), skip: SkipExcludedProject}
		}
		return resolution{root: project.Root, kind: r.kindOf(project.Root), included: true}
	}

	// Rules 3 and 4: a worktree folds into its repository; a repository is
	// its own project.
	if !r.env.exists(dir) {
		if repo, ok := claudeWorktreeRepo(dir); ok {
			return resolution{root: r.env.resolved(repo), kind: ProjectKindRepository}
		}
		if withinAny(dir, r.worktreeStores) {
			return resolution{skip: SkipWorktreeUnresolved}
		}
	}
	if root, ok := r.repositoryRoot(dir); ok {
		return resolution{root: root, kind: ProjectKindRepository}
	}

	// Rule 5: Claude desktop scratch chats share one project, the
	// scratch-workspaces folder.
	if withinAny(dir, r.scratch) {
		return resolution{root: r.env.resolved(r.scratch[0]), kind: ProjectKindScratch}
	}

	// Rule 6: temporary directories.
	if withinAny(dir, r.temps) {
		if r.filters.IncludeTemp {
			return resolution{root: dir, kind: ProjectKindTemporary}
		}
		return resolution{root: dir, kind: ProjectKindTemporary, skip: SkipTemporaryDirectory}
	}

	// Rule 7: home and everything above it. Only home itself can become a
	// project; a folder above it (/, /Users) stays skipped even with the
	// flag, which would otherwise capture every session on the Mac.
	if pathWithin(r.home, dir) || pathWithin(r.homeRaw, dir) {
		if dir == r.home && r.filters.IncludeHome {
			return resolution{root: dir, kind: ProjectKindHome}
		}
		return resolution{root: dir, kind: ProjectKindHome, skip: SkipHomeDirectory}
	}

	// Rule 8: anything else is its own project, whether or not it exists.
	return resolution{root: dir, kind: ProjectKindDirectory}
}

// configuredOwner is the nearest configured project containing dir, compared
// on resolved paths: the rule hooks use (configuredProjectActivationFor).
func (r *resolver) configuredOwner(dir string) (project archiveProject, found bool) {
	bestLen := -1
	for _, p := range r.cfg.Archive.Projects {
		configured := r.env.resolved(p.Root)
		if !pathWithin(dir, configured) || len(configured) <= bestLen {
			continue
		}
		project, bestLen, found = archiveProject{Root: p.Root, Included: p.Included}, len(configured), true
	}
	return project, found
}

type archiveProject struct {
	Root     string
	Included bool
}

// claudeWorktreeRepo maps <repo>/.claude/worktrees/<name>[/...] to <repo> by
// its path alone, for a worktree that no longer exists.
func claudeWorktreeRepo(dir string) (string, bool) {
	marker := string(filepath.Separator) + filepath.Join(".claude", "worktrees") + string(filepath.Separator)
	i := strings.Index(dir, marker)
	if i <= 0 || len(dir) == i+len(marker) {
		return "", false
	}
	return dir[:i], true
}

// repositoryRoot walks up from dir looking for .git. A .git directory makes
// its parent the root. A .git file is a linked worktree (or a submodule):
// gitdir: names its git directory, and a commondir there leads to the main
// repository's .git, whose parent is the root. The walk never reaches home
// or anything above it, so a home-directory repository (dotfiles) does not
// claim every folder under home.
func (r *resolver) repositoryRoot(dir string) (string, bool) {
	for d := dir; ; {
		if pathWithin(r.home, d) || pathWithin(r.homeRaw, d) {
			return "", false
		}
		gitPath := filepath.Join(d, ".git")
		if info, err := r.env.stat(gitPath); err == nil {
			if info.IsDir() {
				return d, true
			}
			return r.env.resolved(r.worktreeMain(d, gitPath)), true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", false
		}
		d = parent
	}
}

// worktreeMain follows a .git file to the main repository. Anything it
// cannot follow leaves the checkout holding the file as its own root.
func (r *resolver) worktreeMain(checkout, gitFile string) string {
	data, err := r.env.readFile(gitFile)
	if err != nil {
		return checkout
	}
	line := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
	gitDir, ok := strings.CutPrefix(line, "gitdir:")
	if !ok {
		return checkout
	}
	gitDir = strings.TrimSpace(gitDir)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(checkout, gitDir)
	}
	common, err := r.env.readFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		// No commondir: a submodule or a separate git directory, not a
		// linked worktree. The checkout is the repository.
		return checkout
	}
	commonDir := strings.TrimSpace(string(common))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(gitDir, commonDir)
	}
	commonDir = filepath.Clean(commonDir)
	if filepath.Base(commonDir) != ".git" {
		// A bare repository has no main checkout to fold into.
		return checkout
	}
	return filepath.Dir(commonDir)
}

// kindOf says what an existing configured root is.
func (r *resolver) kindOf(root string) ProjectKind {
	resolved := r.env.resolved(root)
	switch {
	case resolved == r.env.resolved(r.scratch[0]):
		return ProjectKindScratch
	case resolved == r.home:
		return ProjectKindHome
	case withinAny(resolved, r.temps):
		return ProjectKindTemporary
	case r.env.exists(filepath.Join(resolved, ".git")):
		return ProjectKindRepository
	}
	return ProjectKindDirectory
}

// cursorSlug is the folder name Cursor gives a workspace under
// ~/.cursor/projects: the absolute path without its leading separator, with
// every character other than an ASCII letter or digit replaced by '-'. It
// cannot be reversed reliably, so candidates are converted and compared.
func cursorSlug(path string) string {
	path = strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator))
	return slugName(path)
}

func slugName(name string) string {
	b := []byte(name)
	for i, c := range b {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

// cursorMatcher resolves Cursor project slugs to folders.
type cursorMatcher struct {
	env Environment
	// candidates are folders a slug may name: configured roots, roots and
	// working directories from Claude Code and Codex sessions, and Cursor's
	// own workspaceStorage folders.
	candidates []string
	cache      map[string][]string
}

// cursorWalkBudget bounds how many directories one slug's file system walk
// lists.
const cursorWalkBudget = 4096

func newCursorMatcher(env Environment, candidates []string) *cursorMatcher {
	m := &cursorMatcher{env: env, cache: map[string][]string{}}
	m.candidates = append(m.candidates, candidates...)
	m.candidates = append(m.candidates, cursorWorkspaceFolders(env)...)
	return m
}

// cursorWorkspaceFolders reads the folder of each
// ~/Library/Application Support/Cursor/User/workspaceStorage/*/workspace.json.
func cursorWorkspaceFolders(env Environment) []string {
	storage := filepath.Join(env.Home, "Library", "Application Support", "Cursor", "User", "workspaceStorage")
	entries, err := readDirIfExists(env, storage)
	if err != nil {
		return nil
	}
	var folders []string
	for _, e := range entries {
		if !e.dir {
			continue
		}
		data, err := env.readFile(filepath.Join(storage, e.name, "workspace.json"))
		if err != nil {
			continue
		}
		var ws struct {
			Folder string `json:"folder"`
		}
		if json.Unmarshal(data, &ws) != nil || ws.Folder == "" {
			continue
		}
		u, err := url.Parse(ws.Folder)
		if err != nil || u.Scheme != "file" || !filepath.IsAbs(u.Path) {
			continue
		}
		folders = append(folders, filepath.Clean(u.Path))
	}
	return folders
}

// match returns the one folder slug names, or false when none or several do.
func (m *cursorMatcher) match(slug string) (string, bool) {
	matches, ok := m.cache[slug]
	if !ok {
		seen := map[string]bool{}
		add := func(p string) {
			key := m.env.resolved(p)
			if !seen[key] {
				seen[key] = true
				matches = append(matches, p)
			}
		}
		for _, c := range m.candidates {
			if cursorSlug(c) == slug {
				add(c)
			}
		}
		budget := cursorWalkBudget
		m.walk(string(filepath.Separator), slug, &budget, add)
		m.cache[slug] = matches
	}
	if len(matches) != 1 {
		return "", false
	}
	return matches[0], true
}

// walk finds existing folders under dir whose slug is rest, trying each '-'
// in the slug as a path separator. Each directory level is listed once and
// its entries compared by their own slug, so '.', '_', and '-' in real names
// all match.
func (m *cursorMatcher) walk(dir, rest string, budget *int, found func(string)) {
	if *budget <= 0 {
		return
	}
	*budget--
	entries, err := m.env.readDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := slugName(e.Name())
		switch {
		case name == rest:
			found(filepath.Join(dir, e.Name()))
		case strings.HasPrefix(rest, name+"-"):
			m.walk(filepath.Join(dir, e.Name()), rest[len(name)+1:], budget, found)
		}
	}
}

func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
