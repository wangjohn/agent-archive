package backfill

import (
	"path/filepath"
	"slices"
	"sort"
)

// A plain folder the import adds as a project (ProjectKindDirectory, or a
// temporary directory with --include-temp) owns every folder under it that
// no nearer project owns: hooks capture a new session in the nearest
// configured project containing it (cli.configuredProjectActivationFor). A
// session once run in ~/code would otherwise make every unconfigured
// repository under ~/code captured from then on. So the plan looks inside
// such a folder for what must stay as it is, and the import adds each as an
// excluded project: nested repositories and linked worktrees (a .git folder
// or file), and the desktop apps' workspace folders and temporary
// directories, which have rules of their own. An excluded project is the
// nearest configured one for everything under it, so hooks keep ignoring
// those folders exactly as before the import.
//
// The look is bounded (nestedScanBudget folders listed); when the budget
// runs out, or a folder can't be read, the plan says not every folder was
// checked.

// nestedScanBudget bounds how many folders one project's look lists. A
// test lowers it.
var nestedScanBudget = 5000

// nestedSkipNames are folders not looked inside: their contents are never
// where a session runs, and they are large.
var nestedSkipNames = map[string]bool{".git": true, "node_modules": true, ".venv": true, "venv": true, "__pycache__": true, ".cache": true}

// nestedFolders is what the plan found inside one project it adds.
type nestedFolders struct {
	// KeptOut are the folders the import adds as excluded projects.
	KeptOut []string
	// Complete is false when not every folder could be looked in.
	Complete bool
}

// capturesSubfolders reports whether adding a project of this kind makes
// hooks capture sessions in unconfigured folders under it that are not its
// own: a plain folder, or a temporary directory.
func capturesSubfolders(kind ProjectKind) bool {
	return kind == ProjectKindDirectory || kind == ProjectKindTemporary
}

// findNested looks inside root, a project the plan adds, for the folders the
// import must keep out (see above). skip are roots that are, or will be,
// projects of their own: the configured ones and those the plan adds.
func (r *resolver) findNested(root string, skip []string) nestedFolders {
	out := nestedFolders{Complete: true}
	if !r.env.exists(root) {
		return out
	}
	budget := nestedScanBudget
	queue := []string{root}
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		if budget <= 0 {
			out.Complete = false
			break
		}
		budget--
		entries, err := readDirIfExists(r.env, dir)
		if err != nil {
			out.Complete = false
			continue
		}
		for _, e := range entries {
			// A symlink is not followed: hooks resolve a session's folder
			// through it, so what it points to is not under root.
			if !e.dir || nestedSkipNames[e.name] {
				continue
			}
			path := filepath.Join(dir, e.name)
			switch {
			case slices.Contains(skip, path):
				// A project of its own already, or added by this import.
			case r.isWorkspaceFolder(path) || slices.Contains(r.temps, path):
				out.KeptOut = append(out.KeptOut, path)
			case r.env.exists(filepath.Join(path, ".git")):
				out.KeptOut = append(out.KeptOut, path)
			default:
				queue = append(queue, path)
			}
		}
	}
	sort.Strings(out.KeptOut)
	return out
}

// planNested fills plan.nested for every project the plan adds that
// captures its subfolders.
func planNested(r *resolver, plan *Plan) {
	var skip []string
	for _, p := range r.cfg.Archive.Projects {
		skip = append(skip, uniquePaths(filepath.Clean(p.Root), r.env.resolved(p.Root))...)
	}
	summaries := plan.Projects()
	for _, s := range summaries {
		skip = append(skip, s.Root)
	}
	for _, s := range summaries {
		if s.Included || !capturesSubfolders(s.Kind) {
			continue
		}
		if plan.nested == nil {
			plan.nested = map[string]nestedFolders{}
		}
		plan.nested[s.Root] = r.findNested(s.Root, skip)
	}
}
