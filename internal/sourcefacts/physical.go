package sourcefacts

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ProjectFacts contains canonical physical identity, obtained outside admission locks.
type ProjectFacts struct {
	Cwd  string `json:"cwd"`
	Root string `json:"root"`
}

// ProjectResolver caches one pass's facts and caps aggregate metadata operations.
// A new resolver is required for each pass, so changed Git metadata is revalidated.
type ProjectResolver struct {
	Operations int
	GitBytes   int
	Exhausted  bool
	cache      map[string]projectCache
	observed   map[string]projectSnapshot
}

// NewProjectResolver creates a bounded pass-local resolver.
func NewProjectResolver() *ProjectResolver { return &ProjectResolver{cache: map[string]projectCache{}} }

func (r *ProjectResolver) operation() bool {
	if r.Operations >= 1024 {
		r.Exhausted = true
		return false
	}
	r.Operations++
	return true
}

// Resolve returns physical facts; exhaustion and unavailable mappings fail closed.
func (r *ProjectResolver) Resolve(cwd string) (ProjectFacts, bool) {
	if entry, ok := r.cache[cwd]; ok {
		if r.cacheValid(entry) {
			return entry.facts, true
		}
		if r.Exhausted {
			return ProjectFacts{}, false
		}
		delete(r.cache, cwd)
	}
	r.observed = map[string]projectSnapshot{}
	facts, ok := r.resolve(cwd)
	if ok && len(r.cache) < 1024 {
		r.cache[cwd] = projectCache{facts: facts, metadata: r.observed}
	}
	r.observed = nil
	return facts, ok
}

type projectSnapshot struct {
	info    os.FileInfo
	missing bool
}

type projectCache struct {
	facts    ProjectFacts
	metadata map[string]projectSnapshot
}

func (r *ProjectResolver) lstat(path string) (os.FileInfo, error) {
	if !r.operation() {
		return nil, errors.New("metadata budget exhausted")
	}
	info, err := os.Lstat(path)
	if r.observed != nil && (err == nil || errors.Is(err, os.ErrNotExist)) {
		r.observed[path] = projectSnapshot{info: info, missing: errors.Is(err, os.ErrNotExist)}
	}
	return info, err
}

func (r *ProjectResolver) cacheValid(entry projectCache) bool {
	for path, prior := range entry.metadata {
		if !r.operation() {
			return false
		}
		info, err := os.Lstat(path)
		if prior.missing {
			if !errors.Is(err, os.ErrNotExist) {
				return false
			}
			continue
		}
		if err != nil || !os.SameFile(prior.info, info) || prior.info.Mode() != info.Mode() {
			return false
		}
		if !info.IsDir() && (prior.info.Size() != info.Size() || !prior.info.ModTime().Equal(info.ModTime())) {
			return false
		}
	}
	return true
}

// PhysicalProject resolves the nearest Git root, validated main worktree, or cwd.
func PhysicalProject(cwd string) (ProjectFacts, bool) { return NewProjectResolver().Resolve(cwd) }

func (r *ProjectResolver) resolve(cwd string) (ProjectFacts, bool) {
	if !filepath.IsAbs(cwd) {
		return ProjectFacts{}, false
	}
	resolved, err := r.canonical(cwd)
	if err != nil {
		return ProjectFacts{}, false
	}
	info, err := r.lstat(resolved)
	if err != nil || !info.IsDir() {
		return ProjectFacts{}, false
	}
	facts := ProjectFacts{Cwd: resolved, Root: resolved}
	for d, depth := resolved, 0; depth < 64; depth++ {
		info, e := r.lstat(filepath.Join(d, ".git"))
		if e == nil {
			if info.IsDir() {
				facts.Root = d
				return facts, true
			}
			read := func(path string) ([]byte, error) {
				stamp, e := r.lstat(path)
				if e != nil || !stamp.Mode().IsRegular() {
					return nil, errors.New("invalid Git metadata")
				}
				if !r.operation() || r.GitBytes >= 256*1024 {
					r.Exhausted = true
					return nil, errors.New("git metadata budget exhausted")
				}
				f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
				if e != nil {
					return nil, e
				}
				defer func() { _ = f.Close() }()
				if !r.operation() {
					return nil, errors.New("git metadata budget exhausted")
				}
				i, e := f.Stat()
				if e != nil || !i.Mode().IsRegular() || i.Size() > 4096 || !os.SameFile(stamp, i) {
					return nil, errors.New("invalid Git metadata")
				}
				if !r.operation() {
					return nil, errors.New("git metadata budget exhausted")
				}
				remaining := int64(256*1024 - r.GitBytes)
				if i.Size() > remaining {
					r.Exhausted = true
					return nil, errors.New("git metadata budget exhausted")
				}
				b, e := io.ReadAll(io.LimitReader(f, min(int64(4097), remaining)))
				r.GitBytes += len(b)
				if r.GitBytes > 256*1024 {
					r.Exhausted = true
					return nil, errors.New("git metadata budget exhausted")
				}
				return b, e
			}
			exists := func(path string) bool {
				i, e := r.lstat(path)
				return e == nil && i.IsDir()
			}
			// Registry backlinks may use an absolute spelling through an OS
			// directory alias. Compare the physical path with the already
			// canonical checkout; the canonicalizer still accounts every read.
			readMetadata := func(path string) ([]byte, error) {
				b, err := read(path)
				if err != nil || filepath.Base(path) != "gitdir" {
					return b, err
				}
				backlink, err := r.canonical(strings.TrimSpace(string(b)))
				if err != nil {
					return nil, err
				}
				return []byte(backlink), nil
			}
			main, ok := WorktreeMain(d, readMetadata, exists, true)
			if !ok {
				return ProjectFacts{}, false
			}
			main, e = r.canonical(main)
			if e != nil {
				return ProjectFacts{}, false
			}
			facts.Root = main
			return facts, true
		}
		if !errors.Is(e, os.ErrNotExist) {
			return ProjectFacts{}, false
		}
		parent := filepath.Dir(d)
		if parent == d {
			return facts, true
		}
		d = parent
	}
	return ProjectFacts{}, false
}

// canonical accounts every lstat/readlink used to resolve symlinks.
func (r *ProjectResolver) canonical(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("project path must be absolute")
	}
	pending := strings.Split(filepath.Clean(path), string(filepath.Separator))
	resolved := string(filepath.Separator)
	links := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, part)
		info, e := r.lstat(next)
		if e != nil {
			return "", e
		}
		if info.Mode()&os.ModeSymlink == 0 {
			resolved = next
			continue
		}
		links++
		if links > 40 || !r.operation() {
			return "", errors.New("unavailable symlink mapping")
		}
		target, e := os.Readlink(next)
		if e != nil {
			return "", e
		}
		if filepath.IsAbs(target) {
			resolved = string(filepath.Separator)
		}
		pending = append(strings.Split(target, string(filepath.Separator)), pending...)
	}
	return resolved, nil
}
