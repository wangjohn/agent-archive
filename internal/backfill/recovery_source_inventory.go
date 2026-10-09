package backfill

import (
	"context"
	"crypto/sha256"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

// recoverySourceInventory binds membership and header evidence to the native
// paths inspected during discovery. Directory stamps include absent stores.
// Owned transcripts may append only while their inspected prefix is unchanged,
// including hidden/nonselected sessions that supplied negative ownership facts.
// Reads are bounded and remain outside admission locks. This is an observation
// boundary, not an atomic snapshot or a guarantee against change and reversal.
type recoverySourceInventory struct {
	env    Environment
	stamps map[string]recoveryFileStamp
	// appendable retains the original bounded header prefix and provider root.
	appendable map[string]recoveryHeaderPrefix
	headers    map[string]recoveryHeaderPrefix
	complete   bool
}

const recoverySourceObservationLimit = 65536

// Bound extra renewal I/O independently of path count and existing header bounds.
// Exhaustion requires a new plan; it cannot establish partial uniqueness.
const recoveryHeaderRenewalBytes int64 = 128 << 20

type recoveryHeaderPrefix struct {
	bytes int64
	sum   [32]byte
	root  string
}

type recoveryHeaderReader struct {
	io.ReadCloser
	hash   hash.Hash
	bytes  int64
	valid  bool
	retain func(recoveryHeaderPrefix)
}

func (r *recoveryHeaderReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if r.bytes+int64(n) > headScanLimit {
		r.valid = false
	} else {
		_, _ = r.hash.Write(p[:n])
	}
	r.bytes += int64(n)
	if err != nil && err != io.EOF {
		r.valid = false
	}
	return n, err
}

func (r *recoveryHeaderReader) Close() error {
	err := r.ReadCloser.Close()
	if err == nil && r.valid && r.bytes > 0 {
		var sum [32]byte
		copy(sum[:], r.hash.Sum(nil))
		r.retain(recoveryHeaderPrefix{bytes: r.bytes, sum: sum})
	}
	return err
}

func newRecoverySourceInventory(env Environment) *recoverySourceInventory {
	return &recoverySourceInventory{env: env, stamps: map[string]recoveryFileStamp{}, appendable: map[string]recoveryHeaderPrefix{}, headers: map[string]recoveryHeaderPrefix{}, complete: true}
}

// ownContent binds captured header bytes to their observed regular file and
// provider root. Paths without a completed bounded read keep strict stat checks.
func (i *recoverySourceInventory) ownContent(path string, source fs.FileInfo, roots ...string) {
	stamp, ok := i.stamps[path]
	prefix, read := i.headers[path]
	if !ok || !read || stamp.absent || source == nil || !stamp.info.Mode().IsRegular() || !source.Mode().IsRegular() || !os.SameFile(stamp.info, source) {
		return
	}
	root := filepath.Dir(path)
	if len(roots) != 0 {
		root = roots[0]
	}
	if root == "" {
		return
	}
	prefix.root = root
	i.appendable[path] = prefix
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
	env.Open = func(path string) (io.ReadCloser, error) {
		f, err := i.env.open(path)
		if err != nil {
			return nil, err
		}
		return &recoveryHeaderReader{ReadCloser: f, hash: sha256.New(), valid: true, retain: func(prefix recoveryHeaderPrefix) {
			if _, observed := i.stamps[path]; observed {
				i.headers[path] = prefix
			}
		}}, nil
	}
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
	return i.currentBound(ctx, recoveryHeaderRenewalBytes)
}

func (i *recoverySourceInventory) currentBound(ctx context.Context, remaining int64) bool {
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
		} else if prefix, owned := i.appendable[path]; err != nil || !sameRecoveryMember(before.info, info, owned) {
			return false
		} else if owned && info.Size() > before.info.Size() {
			if ctx.Err() != nil || prefix.bytes <= 0 || prefix.bytes > headScanLimit || prefix.bytes > remaining {
				return false
			}
			remaining -= prefix.bytes
			if !i.prefixCurrent(ctx, path, info, prefix) {
				return false
			}
		}
	}
	return ctx.Err() == nil
}

func (i *recoverySourceInventory) prefixCurrent(ctx context.Context, path string, info fs.FileInfo, prefix recoveryHeaderPrefix) bool {
	if ctx.Err() != nil {
		return false
	}
	snapshot, err := transcriptio.Open(sourcefacts.RootOpener{Root: prefix.root}, path, transcriptio.OpenPolicy{Root: prefix.root, RejectSymlinks: true})
	if err != nil {
		return false
	}
	h := sha256.New()
	n, readErr := io.CopyN(h, snapshot.Reader(ctx), prefix.bytes)
	checkErr := snapshot.Check()
	closeErr := snapshot.Close()
	return ctx.Err() == nil && readErr == nil && checkErr == nil && closeErr == nil && n == prefix.bytes && transcriptio.SameObservation(info, snapshot.SourceInfo()) && string(h.Sum(nil)) == string(prefix.sum[:])
}

// sameRecoveryMember reports whether a path still names the observed member.
// An appendable transcript may grow; a shrink or a same-size content change is
// a rewrite, not an append.
func sameRecoveryMember(before, after fs.FileInfo, appendable bool) bool {
	if !os.SameFile(before, after) || before.Mode() != after.Mode() {
		return false
	}
	if appendable && after.Size() > before.Size() {
		return true
	}
	return before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
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
