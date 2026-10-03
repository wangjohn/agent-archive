package nativesessions

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// FileSystem is the small read-only native filesystem port.
type FileSystem interface {
	DirectoryReader
	transcriptio.Opener
}

// OS implements native reads on the host filesystem.
type OS struct{ transcriptio.OS }

// ReadDir enumerates one directory.
func (OS) ReadDir(path string) ([]fs.DirEntry, error) { return readDirectory(path) }

// Candidate is a compact verified identity, not an archive registration.
type Candidate struct {
	Ref        Ref
	NativeID   string
	Directory  string
	StartedAt  time.Time
	ModifiedAt time.Time
	Stamp      transcriptio.Stamp
}

// Scope limits candidates to a checkout and its descendants.
type Scope struct {
	Directories []string
	All         bool
}

// Limits bounds traversal, header records and cumulative reads.
type Limits struct {
	Files       int
	HeaderBytes int64
	RecordBytes int64
	TotalBytes  int64
	Workers     int
}

// Coverage records identity discovery independently of later label inspection.
type Coverage struct {
	Enumerated       int
	Inspected        int
	InScope          int
	Skipped          int
	IdentityComplete bool
	Reason           string
	ReservedBytes    int64
	ReadBytes        int64
}

// Result retains the bounded candidate catalog for explicit older batches.
type Result struct {
	Candidates []Candidate
	Coverage   Coverage
}

var errSubagent = errors.New("native subagent-only transcript")

// InspectNative requires transcript content to establish the selected identity.
// Import's compatibility parser deliberately retains its separate semantics.
func InspectNative(ctx context.Context, s *transcriptio.Snapshot, ref Ref, window, record int64) (Header, transcriptio.RecordWindow, error) {
	if ref.Harness == "codex" {
		var w transcriptio.RecordWindow
		var subagent bool
		h, err := Inspect(ref.Harness, ref.Path, func(visit func([]byte) bool) error {
			var err error
			w, err = s.Records(ctx, false, window, record, func(line []byte) bool {
				var meta struct {
					Type    string `json:"type"`
					Payload struct {
						Source json.RawMessage `json:"source"`
					} `json:"payload"`
				}
				if json.Unmarshal(line, &meta) == nil && meta.Type == "session_meta" {
					var source struct {
						Subagent json.RawMessage `json:"subagent"`
					}
					if json.Unmarshal(meta.Payload.Source, &source) == nil && len(source.Subagent) > 0 {
						subagent = true
					}
				}
				return visit(line)
			})
			return err
		})
		if subagent && err == nil {
			err = errSubagent
		}
		return h, w, err
	}
	var h Header
	var sidechain bool
	w, err := s.Records(ctx, false, window, record, func(line []byte) bool {
		var r struct {
			ID        string `json:"sessionId"`
			Cwd       string `json:"cwd"`
			Timestamp string `json:"timestamp"`
			Sidechain bool   `json:"isSidechain"`
		}
		if json.Unmarshal(line, &r) != nil {
			return true
		}
		if r.Sidechain {
			sidechain = true
			return true
		}
		if r.ID != "" {
			if h.NativeID != "" && h.NativeID != r.ID {
				h.IdentityMismatch = true
			}
			h.NativeID = r.ID
		}
		if h.Directory == "" {
			h.Directory = r.Cwd
		}
		if h.StartedAt.IsZero() {
			h.StartedAt, _ = time.Parse(time.RFC3339Nano, r.Timestamp)
		}
		return true
	})
	if h.NativeID == "" && sidechain && err == nil {
		return h, w, errSubagent
	}
	if h.NativeID == "" || h.NativeID != strings.TrimSuffix(filepath.Base(ref.Path), ".jsonl") {
		h.IdentityMismatch = true
	}
	return h, w, err
}

// Discover enumerates bounded references and inspects them with a fixed pool.
// Only the coordinator canonicalizes checkout paths or mutates the catalog.
func Discover(ctx context.Context, files FileSystem, roots []StoreRoot, scope Scope, limits Limits) (Result, error) {
	var out Result
	if files == nil || limits.Files <= 0 || limits.HeaderBytes <= 0 || limits.RecordBytes <= 0 || limits.TotalBytes <= 0 || limits.Workers <= 0 {
		return out, errors.New("native discovery requires filesystem and positive limits")
	}
	refs, coverage, err := enumerateNativeRefs(ctx, files, roots, limits.Files)
	if err != nil {
		return out, err
	}
	out.Coverage = coverage
	jobs, err := reserveNativeHeaders(ctx, files, refs, limits, &out.Coverage)
	if err != nil {
		return out, err
	}
	answers, err := inspectNativeHeaders(ctx, files, jobs, limits)
	if err != nil {
		return out, err
	}
	// All header jobs have finished, so unused worst-case reservations can be
	// released without making scheduling depend on worker completion order.
	for _, a := range answers {
		out.Coverage.ReadBytes += a.bytes
	}
	out.Coverage.ReservedBytes = out.Coverage.ReadBytes
	out.Candidates, err = scopeNativeCandidates(ctx, files, scope, answers, &out.Coverage)
	out.Coverage.InScope = len(out.Candidates)
	if !out.Coverage.IdentityComplete && out.Coverage.Reason == "" {
		out.Coverage.Reason = "unverified identities or checkouts"
	}
	sort.Slice(out.Candidates, func(i, j int) bool { return candidateBefore(out.Candidates[i], out.Candidates[j]) })
	return out, err
}

func candidateBefore(a, b Candidate) bool {
	if !a.ModifiedAt.Equal(b.ModifiedAt) {
		return a.ModifiedAt.After(b.ModifiedAt)
	}
	if a.Ref.Harness != b.Ref.Harness {
		return a.Ref.Harness < b.Ref.Harness
	}
	if a.NativeID != b.NativeID {
		return a.NativeID < b.NativeID
	}
	return a.Ref.Path < b.Ref.Path
}

func enumerateNativeRefs(ctx context.Context, files FileSystem, roots []StoreRoot, capFiles int) ([]Ref, Coverage, error) {
	c := Coverage{IdentityComplete: true}
	roots = append([]StoreRoot(nil), roots...)
	sort.Slice(roots, func(i, j int) bool {
		if roots[i].Harness != roots[j].Harness {
			return roots[i].Harness < roots[j].Harness
		}
		return roots[i].Path < roots[j].Path
	})
	var refs []Ref
	stores := map[string]bool{}
	seen := map[string]bool{}
	for _, root := range roots {
		canonical, err := files.EvalSymlinks(root.Path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				c.IdentityComplete = false
				c.Reason = "unreadable store"
			}
			continue
		}
		root.Path = canonical
		key := root.Harness + "\x00" + canonical
		if stores[key] {
			continue
		}
		stores[key] = true
		remaining := capFiles - len(refs)
		if remaining <= 0 {
			c.IdentityComplete = false
			c.Reason = "file limit"
			break
		}
		coverage, err := Walk(ctx, files, root, remaining, func(ref Ref) (bool, error) {
			key := ref.Harness + "\x00" + ref.Path
			if !seen[key] {
				seen[key] = true
				refs = append(refs, ref)
			}
			return len(refs) < capFiles, nil
		})
		if err != nil {
			return refs, c, err
		}
		if !coverage.Complete {
			c.IdentityComplete = false
			c.Reason = "incomplete enumeration"
		}
	}
	c.Enumerated = len(refs)
	return refs, c, nil
}

type headerJob struct {
	index  int
	ref    Ref
	window int64
}

type headerAnswer struct {
	c     Candidate
	bytes int64
	err   error
}

func reserveNativeHeaders(ctx context.Context, files FileSystem, refs []Ref, limits Limits, c *Coverage) ([]headerJob, error) {
	var jobs []headerJob
	for i, ref := range refs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := files.Lstat(ref.Path)
		if err != nil {
			c.Skipped++
			c.IdentityComplete = false
			continue
		}
		cost := min(max(0, info.Size()), limits.HeaderBytes)
		if cost > limits.TotalBytes-c.ReservedBytes {
			c.IdentityComplete = false
			c.Reason = "read budget"
			break
		}
		c.ReservedBytes += cost
		jobs = append(jobs, headerJob{index: i, ref: ref, window: cost})
	}
	return jobs, nil
}

func inspectNativeHeader(ctx context.Context, files FileSystem, j headerJob, limits Limits) headerAnswer {
	s, err := transcriptio.Open(files, j.ref.Path, transcriptio.OpenPolicy{RejectSymlinks: true, Root: j.ref.Store})
	if err != nil {
		return headerAnswer{err: err}
	}
	defer func() { _ = s.Close() }()
	h, w, err := InspectNative(ctx, s, j.ref, j.window, limits.RecordBytes)
	if err == nil && (h.IdentityMismatch || !validNativeID(h.NativeID) || !filepath.IsAbs(h.Directory)) {
		err = errors.New("native identity or checkout unavailable")
	}
	var c Candidate
	if err == nil {
		stamp := s.Stamp()
		c = Candidate{Ref: j.ref, NativeID: h.NativeID, Directory: h.Directory, StartedAt: h.StartedAt, ModifiedAt: stamp.ModifiedAt, Stamp: stamp}
	}
	return headerAnswer{c: c, bytes: w.Bytes, err: err}

}

func validNativeID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func inspectNativeHeaders(ctx context.Context, files FileSystem, scheduled []headerJob, limits Limits) ([]headerAnswer, error) {
	type indexedAnswer struct {
		index  int
		answer headerAnswer
	}
	jobs := make(chan headerJob, limits.Workers)
	results := make(chan indexedAnswer, limits.Workers)
	var workers sync.WaitGroup
	for range limits.Workers {
		workers.Go(func() {
			for j := range jobs {
				a := inspectNativeHeader(ctx, files, j, limits)
				select {
				case results <- indexedAnswer{index: j.index, answer: a}:
				case <-ctx.Done():
					return
				}
			}
		})
	}
	go func() {
		defer close(jobs)
		for _, j := range scheduled {
			select {
			case jobs <- j:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { workers.Wait(); close(results) }()
	got := map[int]headerAnswer{}
	for a := range results {
		got[a.index] = a.answer
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	answers := make([]headerAnswer, 0, len(scheduled))
	for _, j := range scheduled {
		answers = append(answers, got[j.index])
	}
	return answers, nil
}

func scopeNativeCandidates(ctx context.Context, files FileSystem, scope Scope, answers []headerAnswer, c *Coverage) ([]Candidate, error) {
	cache := map[string]string{}
	canonical := func(dir string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if d, ok := cache[dir]; ok {
			return d, nil
		}
		d, err := files.EvalSymlinks(dir)
		if canceled := ctx.Err(); canceled != nil {
			return "", canceled
		}
		if err != nil {
			d = ""
		}
		cache[dir] = d
		return d, nil
	}
	var dirs []string
	for _, dir := range scope.Directories {
		d, err := canonical(dir)
		if err != nil {
			return nil, err
		}
		if d != "" {
			dirs = append(dirs, d)
		}
	}
	if !scope.All && len(dirs) == 0 {
		return nil, errors.New("checkout directory cannot be resolved; use --project DIR")
	}
	var candidates []Candidate
	for _, a := range answers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.Inspected++
		if a.err != nil {
			c.Skipped++
			if !errors.Is(a.err, errSubagent) {
				c.IdentityComplete = false
			}
			continue
		}
		var err error
		a.c.Directory, err = canonical(a.c.Directory)
		if err != nil {
			return nil, err
		}
		if a.c.Directory == "" {
			c.Skipped++
			c.IdentityComplete = false
			continue
		}
		if directoryInScope(a.c.Directory, dirs, scope.All) {
			candidates = append(candidates, a.c)
		}
	}
	return candidates, nil
}

func directoryInScope(candidate string, dirs []string, all bool) bool {
	if all {
		return true
	}
	for _, dir := range dirs {
		if local.PathWithin(candidate, dir) {
			return true
		}
	}

	return false
}
