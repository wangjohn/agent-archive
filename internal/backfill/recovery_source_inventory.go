package backfill

import (
	"context"
	"io/fs"
	"os"
	"sort"
)

// recoverySourceInventory binds discovered membership and headers to the native
// paths inspected during discovery. It never reopens unchanged transcripts.
// Directory stamps include absent stores, so new files/clones require a new plan.
// These are practical observations, with the same timestamp-restoration limit as
// the existing native file checks, not an atomic filesystem snapshot.
type recoverySourceInventory struct {
	env      Environment
	stamps   map[string]recoveryFileStamp
	complete bool
}

const recoverySourceObservationLimit = 65536

func newRecoverySourceInventory(env Environment) *recoverySourceInventory {
	return &recoverySourceInventory{env: env, stamps: map[string]recoveryFileStamp{}, complete: true}
}

func (i *recoverySourceInventory) observe(path string) {
	if _, ok := i.stamps[path]; ok {
		return
	}
	if len(i.stamps) >= recoverySourceObservationLimit {
		i.complete = false
		return
	}
	info, err := i.env.lstat(path)
	if os.IsNotExist(err) {
		i.stamps[path] = recoveryFileStamp{absent: true}
	} else if err != nil {
		i.complete = false
	} else {
		i.stamps[path] = recoveryFileStamp{info: info}
	}
}

func (i *recoverySourceInventory) environment() Environment {
	env := i.env
	env.ReadDir = func(path string) ([]fs.DirEntry, error) {
		i.observe(path)
		return i.env.readDir(path)
	}
	env.Lstat = func(path string) (fs.FileInfo, error) {
		i.observe(path)
		return i.env.lstat(path)
	}
	return env
}

func (i *recoverySourceInventory) current(ctx context.Context) bool {
	if !i.complete || ctx.Err() != nil {
		return false
	}
	paths := make([]string, 0, len(i.stamps))
	for path := range i.stamps {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if ctx.Err() != nil {
			return false
		}
		before := i.stamps[path]
		info, err := i.env.lstat(path)
		if before.absent {
			if !os.IsNotExist(err) {
				return false
			}
		} else if err != nil || !os.SameFile(before.info, info) || before.info.Mode() != info.Mode() || before.info.Size() != info.Size() || !before.info.ModTime().Equal(info.ModTime()) {
			return false
		}
	}
	return ctx.Err() == nil
}

// recoveryOwnershipCurrent renews all discovered cwd classifications, including
// plain folders and absent paths that could acquire a live checkout. Git identity
// validation still belongs to the separate bounded repository inventory.
func recoveryOwnershipCurrent(r *resolver, items []*work) func(context.Context) bool {
	type ownershipFact struct {
		path             string
		res              resolution
		exists           bool
		workspaceCurrent func(context.Context) bool
	}
	facts := make([]ownershipFact, 0, min(len(items), recoverySourceObservationLimit))
	complete := len(items) <= recoverySourceObservationLimit
	for _, w := range items[:min(len(items), recoverySourceObservationLimit)] {
		path := w.t.cwd
		if path == "" {
			path = w.res.root
		}
		if path != "" {
			facts = append(facts, ownershipFact{path: path, res: w.res, exists: r.env.exists(path), workspaceCurrent: w.workspaceCurrent})
		}
	}
	return func(ctx context.Context) bool {
		if !complete || ctx.Err() != nil {
			return false
		}
		ownership := newResolver(r.env, r.cfg, r.filters)
		for _, fact := range facts {
			if ctx.Err() != nil || r.env.exists(fact.path) != fact.exists {
				return false
			}
			fresh := ownership.resolve(fact.path)
			if fresh.root != fact.res.root || fresh.kind != fact.res.kind || fresh.skip != fact.res.skip {
				return false
			}
			if fact.workspaceCurrent != nil && !fact.workspaceCurrent(ctx) {
				return false
			}
		}
		return ctx.Err() == nil
	}
}
