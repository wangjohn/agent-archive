// Package rolloutcatalog supplies bounded, pass-local Codex source evidence.
// Native stores are authority to read files, never permission to publish content.
package rolloutcatalog

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

// Limits bounds inventory independently of the reader's graph and raw budgets.
// Zero values select conservative defaults; a catalog is serial and single-owner.
type Limits struct {
	Roots           int
	Entries         int
	Directories     int
	CheckOperations int
	HeaderBytes     int64
	PrefixBytes     int64
	Records         int
}

// Counters reports operations without native paths or identities.
type Counters struct {
	// Stats counts explicit filesystem stat calls, including descriptor stats.
	Stats int
	// RootOpens counts attempts to open confined directory descriptors.
	RootOpens int
	// FileOpens counts attempts to open regular files or inventory directories.
	FileOpens int
	// Resolutions counts EvalSymlinks calls, whose internal syscalls are not counted.
	Resolutions int
	// MetadataJoins counts entry visits for identity and thread/native indexes.
	MetadataJoins int
	// FileBytes counts bytes actually returned by transcript file ReadAt calls.
	FileBytes int64

	ValidationSweeps int
	Entries          int
	Directories      int
	Headers          int
	Checks           int
	NativeQueries    int
	CheckOperations  int
	PrefixRecords    int
	HeaderBytes      int64
	PrefixBytes      int64
	NativeBytes      int64
	CheckBytes       int64
}

// Catalog implements one immutable observation epoch. Renew it after interruption.
// Failed enumeration remains incomplete; it is never a discovery cache.
type Catalog struct {
	homes             []string
	limits            Limits
	started           bool
	complete          bool
	invalid           bool
	counters          Counters
	issues            map[string]int
	files             []*entry
	dirs              []directory
	threads           map[string][]agentapi.SourceRef
	rollouts          map[string][]agentapi.SourceRef
	current           map[string]*agentapi.SourceRef
	revisions         map[string]string
	native            []nativeObservation
	authorities       []authority
	authorityReady    bool
	authorityFailures int
}

type directory struct {
	root    string
	path    string
	info    fs.FileInfo
	missing bool
}

type entry struct {
	root      string
	ref       agentapi.SourceRef
	info      fs.FileInfo
	identity  codexmeta.CodexIdentity
	meta      codexmeta.CodexMeta
	headerLen int64
	header    [32]byte
	prefixLen int64
	prefix    [32]byte
	duplicate bool
}

type threadOrigin string

const (
	threadUser     threadOrigin = "user"
	threadSubagent threadOrigin = "subagent"
)

// New creates a lazy catalog from explicitly approved homes, without reading HOME.
// Homes are source authority, not a namespace for logical thread IDs.
func New(homes []string, limits Limits) *Catalog {
	if limits.Roots <= 0 {
		limits.Roots = 16
	}
	if limits.Entries <= 0 {
		limits.Entries = 16384
	}
	if limits.Directories <= 0 {
		limits.Directories = 2048
	}
	if limits.HeaderBytes <= 0 {
		limits.HeaderBytes = 32 << 20
	}
	if limits.PrefixBytes <= 0 {
		limits.PrefixBytes = 128 << 20
	}
	if limits.CheckOperations <= 0 {
		limits.CheckOperations = 1 << 20
	}
	if limits.Records <= 0 {
		limits.Records = 1000000
	}
	return &Catalog{homes: slices.Clone(homes), limits: limits, issues: map[string]int{}, threads: map[string][]agentapi.SourceRef{}, rollouts: map[string][]agentapi.SourceRef{}, current: map[string]*agentapi.SourceRef{}, revisions: map[string]string{}}
}

// Counters returns this epoch's content-free work measurements.
func (c *Catalog) Counters() Counters { return c.counters }

// Issues returns fixed-vocabulary uncertainty counts without native locators.
func (c *Catalog) Issues() map[string]int {
	out := map[string]int{}
	maps.Copy(out, c.issues)
	return out
}

func (c *Catalog) fail(code string) { c.complete = false; c.issues[code]++ }

func failure(message string) error { return agentapi.Wrap(agentapi.Unavailable, errors.New(message)) }

// Thread returns stable thread evidence across all approved homes.
func (c *Catalog) Thread(ctx context.Context, id string) (agentapi.CodexRolloutSet, error) {
	if err := c.ensure(ctx); err != nil {
		return agentapi.CodexRolloutSet{}, err
	}
	if c.invalid {
		return agentapi.CodexRolloutSet{}, failure("rollout catalog changed; renew the observation epoch")
	}
	key := strings.ToLower(id)
	current := c.current[key]
	if !c.complete {
		current = nil
	}
	return agentapi.CodexRolloutSet{Current: cloneRef(current), Candidates: slices.Clone(c.threads[key]), Revision: c.revisions[key], Complete: c.complete}, nil
}

func cloneRef(ref *agentapi.SourceRef) *agentapi.SourceRef {
	if ref == nil {
		return nil
	}
	cloned := *ref
	return &cloned
}

// Rollout returns all conflicting physical copies, coalescing only verified prefixes.
func (c *Catalog) Rollout(ctx context.Context, id string) ([]agentapi.SourceRef, error) {
	if err := c.ensure(ctx); err != nil {
		return nil, err
	}
	if c.invalid {
		return nil, failure("rollout catalog changed")
	}
	return slices.Clone(c.rollouts[strings.ToLower(id)]), nil
}

func (c *Catalog) ensure(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		c.fail("cancelled")
		c.invalid = true
		return err
	}
	if c.started {
		return nil
	}
	c.started = true
	c.complete = true
	if len(c.homes) == 0 || len(c.homes) > c.limits.Roots {
		c.fail("root_budget")
		return nil
	}
	c.initializeAuthority()
	if c.authorityFailures > 0 {
		c.fail("root_unavailable")
	}
	roots := map[string]bool{}
	for _, approved := range c.authorities {
		home := approved.home
		if err := ctx.Err(); err != nil {
			c.fail("cancelled")
			c.invalid = true
			return err
		}
		if !filepath.IsAbs(home) || len(home) > 8192 {
			c.fail("root_unavailable")
			continue
		}
		measureResolution(&c.counters)
		root, err := filepath.EvalSymlinks(home)
		if err != nil {
			c.fail("root_unavailable")
			continue
		}
		if root != approved.root {
			c.fail("root_changed")
			continue
		}
		if roots[root] {
			continue
		}
		roots[root] = true
		measureRootOpen(&c.counters)
		opened, err := os.OpenRoot(root)
		if err != nil {
			c.fail("root_unavailable")
			continue
		}
		measureStat(&c.counters)
		info, err := opened.Stat(".")
		if err != nil || !info.IsDir() || !os.SameFile(info, approved.info) {
			c.fail("root_unavailable")
			_ = opened.Close()
			continue
		}
		c.dirs = append(c.dirs, directory{root: root, path: root, info: info})
		for _, store := range []string{"sessions", "archived_sessions"} {
			c.walk(ctx, opened, root, store, 0)
		}
		_ = opened.Close()
	}
	c.coalesce(ctx)
	for _, root := range sortedKeys(roots) {
		c.observeCurrent(ctx, root)
	}
	// Completion includes a final membership fence across every visited directory,
	// rather than merely successful individual subtree walks.
	for _, d := range c.dirs {
		info, err := measuredLstat(&c.counters, d.root, d.path)
		if d.missing {
			if !errors.Is(err, fs.ErrNotExist) {
				c.fail("membership_changed")
			}
		} else if err != nil || !sameDirectory(d.info, info) {
			c.fail("membership_changed")
		}
	}
	evidenceByThread := map[string][]string{}
	for _, e := range c.files {
		c.counters.MetadataJoins++
		id := strings.ToLower(e.identity.ThreadID)
		evidenceByThread[id] = append(evidenceByThread[id], e.ref.Path+":"+hex.EncodeToString(e.header[:])+":"+hex.EncodeToString(e.prefix[:]))
	}
	for id, refs := range c.threads {
		evidence := evidenceByThread[id]
		slices.Sort(evidence)
		raw, _ := json.Marshal(struct {
			Refs     []agentapi.SourceRef `json:"Refs"`
			Current  *agentapi.SourceRef  `json:"Current"`
			Complete bool                 `json:"Complete"`
			Evidence []string             `json:"Evidence"`
		}{refs, c.current[id], c.complete, evidence})
		digest := sha256.Sum256(raw)
		c.revisions[id] = hex.EncodeToString(digest[:])
	}
	if err := ctx.Err(); err != nil {
		c.fail("cancelled")
		c.invalid = true
		return err
	}
	return nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func (c *Catalog) walk(ctx context.Context, opened *os.Root, root, relative string, depth int) {
	if ctx.Err() != nil {
		c.fail("cancelled")
		return
	}
	if depth > 64 || c.counters.Directories >= c.limits.Directories {
		c.fail("directory_budget")
		return
	}
	path := filepath.Join(root, relative)
	measureStat(&c.counters)
	info, err := opened.Lstat(relative)
	if errors.Is(err, fs.ErrNotExist) {
		c.dirs = append(c.dirs, directory{root: root, path: path, missing: true})
		return
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		c.fail("store_unavailable")
		return
	}
	c.counters.Directories++
	c.dirs = append(c.dirs, directory{root: root, path: path, info: info})
	measureFileOpen(&c.counters)
	dir, err := opened.Open(relative)
	if err != nil {
		c.fail("store_unavailable")
		return
	}
	defer func() { _ = dir.Close() }()
	for {
		entries, err := dir.ReadDir(128)
		for _, item := range entries {
			if ctx.Err() != nil {
				c.fail("cancelled")
				return
			}
			if c.counters.Entries >= c.limits.Entries {
				c.fail("entry_budget")
				return
			}
			c.counters.Entries++
			child := filepath.Join(relative, item.Name())
			if item.IsDir() {
				c.walk(ctx, opened, root, child, depth+1)
				continue
			}
			if item.Type()&os.ModeSymlink != 0 {
				c.fail("unsafe_entry")
				continue
			}
			if !strings.HasSuffix(item.Name(), ".jsonl") {
				continue
			}
			c.readHeader(ctx, root, filepath.Join(root, child))
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			c.fail("store_unavailable")
			break
		}
	}
	measureStat(&c.counters)
	after, err := opened.Lstat(relative)
	if err != nil || !sameDirectory(info, after) {
		c.fail("membership_changed")
	}
}

func sameDirectory(a, b fs.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func (c *Catalog) readHeader(ctx context.Context, root, path string) {
	if c.counters.HeaderBytes >= c.limits.HeaderBytes {
		c.fail("header_budget")
		return
	}
	f, err := transcriptio.Open(c.Files(), path, transcriptio.OpenPolicy{RejectSymlinks: true})
	if err != nil {
		c.fail("source_unavailable")
		return
	}
	defer func() { _ = f.Close() }()
	line, err := bufio.NewReader(io.TeeReader(io.LimitReader(f.Reader(ctx), min(int64(65537), c.limits.HeaderBytes-c.counters.HeaderBytes)), countedRead{Writer: io.Discard, bytes: &c.counters.HeaderBytes})).ReadBytes('\n')
	c.counters.Headers++
	if err != nil || len(line) > 65536 {
		c.fail("header_unavailable")
		return
	}
	meta, started, found, err := codexmeta.ParseCodexMeta(line)
	if err != nil || !found {
		c.fail("invalid_header")
		return
	}
	meta.Timestamp = started.Format("2006-01-02T15:04:05.999999999Z07:00")
	identity, outcome := meta.Identity(path)
	if !codexmeta.ValidCodexVersion(meta.Version) || strings.TrimSpace(meta.Originator) == "" || len(meta.Originator) > 256 || strings.ContainsFunc(meta.Originator, unicode.IsControl) {
		c.fail("unknown_identity")
		return
	}
	var source string
	if json.Unmarshal(meta.Source, &source) == nil && !codexmeta.ValidCodexExecutionSource(source) {
		c.fail("unknown_identity")
		return
	}
	var threadSource string
	if len(meta.ThreadSource) > 0 && (json.Unmarshal(meta.ThreadSource, &threadSource) != nil || (threadOrigin(threadSource) != threadUser && threadOrigin(threadSource) != threadSubagent)) {
		c.fail("unknown_identity")
		return
	}
	if len(meta.Source) == 0 {
		c.fail("unknown_identity")
		return
	}
	if outcome != "" || (identity.HistoryMode != "" && identity.HistoryMode != codexmeta.CodexHistoryLegacy && identity.HistoryMode != codexmeta.CodexHistoryPaginated) {
		c.fail("unknown_identity")
		return
	}
	if f.Check() != nil || ctx.Err() != nil {
		c.fail("source_changed")
		return
	}
	c.files = append(c.files, &entry{root: root, ref: agentapi.SourceRef{Kind: archive.SourceKindFile, Path: path}, info: f.SourceInfo(), identity: identity, meta: meta, headerLen: int64(len(line)), header: sha256.Sum256(line)})
}

func stableFacts(e *entry, physical bool) string {
	id := e.identity
	if !physical {
		id.RolloutID = ""
		id.HistoryBase = nil
		id.HistoryMode = ""
	}
	b, _ := json.Marshal(struct {
		Identity codexmeta.CodexIdentity `json:"Identity"`
		Created  string                  `json:"Created"`
		Cwd      string                  `json:"Cwd"`
		Producer string                  `json:"Producer"`
		Version  string                  `json:"Version"`
		Source   json.RawMessage         `json:"Source"`
		Git      codexmeta.GitInfo       `json:"Git"`
	}{id, e.meta.Timestamp, e.meta.Cwd, e.meta.Originator, e.meta.Version, e.meta.Source, e.meta.Git})
	return string(b)
}

func (c *Catalog) coalesce(ctx context.Context) {
	groups := map[string][]*entry{}
	threadFacts := map[string]string{}
	for _, e := range c.files {
		c.counters.MetadataJoins++
		rid := strings.ToLower(e.identity.RolloutID)
		groups[rid] = append(groups[rid], e)
		tid := strings.ToLower(e.identity.ThreadID)
		facts := stableFacts(e, false)
		if prior, ok := threadFacts[tid]; ok && prior != facts {
			c.fail("identity_conflict")
		}
		threadFacts[tid] = facts
	}
	for _, rid := range sortedEntryKeys(groups) {
		group := groups[rid]
		if len(group) > 1 {
			for _, e := range group {
				e.duplicate = true
			}
		}
		slices.SortFunc(group, func(a, b *entry) int {
			if a.info.Size() > b.info.Size() {
				return -1
			}
			if a.info.Size() < b.info.Size() {
				return 1
			}
			return strings.Compare(a.ref.Path, b.ref.Path)
		})
		agree := true
		for _, e := range group[1:] {
			if stableFacts(e, true) != stableFacts(group[0], true) || !c.agreePrefix(ctx, group[0], e) {
				agree = false
			}
		}
		selected := group
		if agree {
			selected = group[:1]
		} else {
			c.fail("physical_conflict")
		}
		for _, e := range selected {
			c.rollouts[rid] = append(c.rollouts[rid], e.ref)
			tid := strings.ToLower(e.identity.ThreadID)
			c.threads[tid] = append(c.threads[tid], e.ref)
		}
	}
	for id := range c.threads {
		slices.SortFunc(c.threads[id], func(a, b agentapi.SourceRef) int { return strings.Compare(a.Path, b.Path) })
	}
}

func sortedEntryKeys(m map[string][]*entry) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func (c *Catalog) prefix(ctx context.Context, e *entry, n int64) ([32]byte, bool) {
	if n <= 0 || n > c.limits.PrefixBytes-c.counters.PrefixBytes {
		c.fail("prefix_budget")
		return [32]byte{}, false
	}
	f, err := transcriptio.Open(c.Files(), e.ref.Path, transcriptio.OpenPolicy{RejectSymlinks: true})
	if err != nil {
		c.fail("source_unavailable")
		return [32]byte{}, false
	}
	defer func() { _ = f.Close() }()
	// Prefix bytes must come from the same observation as the parsed identity.
	// A freshly opened, internally consistent replacement is insufficient proof.
	if !transcriptio.SameObservation(e.info, f.SourceInfo()) {
		c.fail("source_changed")
		return [32]byte{}, false
	}
	h := sha256.New()
	reader := bufio.NewReader(io.TeeReader(io.LimitReader(f.Reader(ctx), n), countedRead{Writer: h, bytes: &c.counters.PrefixBytes}))
	var bytes int64
	records := 0
	for {
		line, err := reader.ReadSlice('\n')
		bytes += int64(len(line))
		if err == nil {
			records++
			c.counters.PrefixRecords++
		}
		if records > c.limits.Records || c.counters.PrefixRecords > c.limits.Records {
			c.fail("record_budget")
			return [32]byte{}, false
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return [32]byte{}, false
		}
	}
	if bytes != n || f.Check() != nil || ctx.Err() != nil {
		c.fail("source_changed")
		return [32]byte{}, false
	}
	named, err := c.Files().Lstat(e.ref.Path)
	if err != nil || !transcriptio.SameObservation(e.info, named) {
		c.fail("source_changed")
		return [32]byte{}, false
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, true
}

// countedRead charges native bytes actually read, including buffered lookahead
// on cancelled or record-budget failure paths.
type countedRead struct {
	io.Writer
	bytes *int64
}

func (w countedRead) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	*w.bytes += int64(n)
	return n, err
}

func (c *Catalog) agreePrefix(ctx context.Context, a, b *entry) bool {
	n := min(a.info.Size(), b.info.Size())
	x, ok := c.prefix(ctx, a, n)
	if !ok {
		return false
	}
	y, ok := c.prefix(ctx, b, n)
	if !ok {
		return false
	}
	if x != y {
		return false
	}
	for _, e := range []*entry{a, b} {
		if n >= e.prefixLen {
			e.prefixLen = n
			e.prefix = x
		}
	}
	return true
}

// Check fences membership, header identity, observed duplicate prefixes and current
// evidence. It stats shared observations; it never enumerates a home per thread.
func (c *Catalog) Check(ctx context.Context, id, revision string) error {
	if err := c.ensure(ctx); err != nil {
		return err
	}
	c.counters.Checks++
	if c.invalid || revision == "" || c.revisions[strings.ToLower(id)] != revision {
		return failure("rollout selection epoch unavailable")
	}
	return c.validate(ctx, false)
}

func (c *Catalog) validate(ctx context.Context, boundedSlice bool) error {
	startOperations := c.counters.CheckOperations
	startBytes := c.counters.CheckBytes
	operation := func() error {
		if !boundedSlice {
			return c.checkOperation(ctx)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.counters.CheckOperations-startOperations >= c.limits.CheckOperations {
			return agentapi.Wrap(agentapi.Limit, errors.New("catalog slice validation operation budget exhausted"))
		}
		c.counters.CheckOperations++
		return nil
	}
	c.counters.ValidationSweeps++
	changed := func() error {
		c.invalid = true
		c.fail("epoch_changed")
		return agentapi.Wrap(agentapi.Changed, errors.New("rollout evidence changed; renew the catalog"))
	}
	for _, approved := range c.authorities {
		if err := operation(); err != nil {
			return err
		}
		measureResolution(&c.counters)
		resolved, err := filepath.EvalSymlinks(approved.home)
		if err != nil || resolved != approved.root {
			return changed()
		}
		measureStat(&c.counters)
		info, err := os.Lstat(approved.root)
		if err != nil || !info.IsDir() || !os.SameFile(approved.info, info) {
			return changed()
		}
	}
	for _, d := range c.dirs {
		if err := operation(); err != nil {
			return err
		}
		info, err := measuredLstat(&c.counters, d.root, d.path)
		if d.missing {
			if !errors.Is(err, fs.ErrNotExist) {
				return changed()
			}
			continue
		}
		if err != nil || !sameDirectory(d.info, info) {
			return changed()
		}
	}
	for _, e := range c.files {
		if err := operation(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			if !boundedSlice {
				c.invalid = true
				c.fail("cancelled")
			}
			return err
		}
		info, err := measuredLstat(&c.counters, e.root, e.ref.Path)
		if err != nil || !os.SameFile(e.info, info) || info.Size() < e.info.Size() {
			return changed()
		}
		if e.duplicate && info.Size() != e.info.Size() {
			return changed()
		}
		if transcriptio.SameObservation(e.info, info) {
			continue
		}
		charge := e.headerLen + e.prefixLen
		usedBytes := c.counters.CheckBytes
		if boundedSlice {
			usedBytes -= startBytes
		}
		if charge > c.limits.PrefixBytes-usedBytes {
			if !boundedSlice {
				c.invalid = true
				c.fail("check_budget")
			}
			return agentapi.Wrap(agentapi.Limit, errors.New("catalog prefix revalidation budget exhausted"))
		}
		c.counters.CheckBytes += charge
		f, err := transcriptio.Open(c.Files(), e.ref.Path, transcriptio.OpenPolicy{RejectSymlinks: true})
		if err != nil {
			return changed()
		}
		if !transcriptio.SameObservation(info, f.SourceInfo()) {
			_ = f.Close()
			return changed()
		}
		err = f.CheckPrefix(ctx, e.headerLen, e.header)
		if err == nil && e.prefixLen > 0 {
			err = f.CheckPrefix(ctx, e.prefixLen, e.prefix)
		}
		err = errors.Join(err, f.Check(), f.Close())
		if err != nil {
			if ctx.Err() != nil && boundedSlice {
				return ctx.Err()
			}
			return changed()
		}
		e.info = info
	}

	for _, n := range c.native {
		if err := operation(); err != nil {
			return err
		}
		if !n.check() {
			return changed()
		}
	}
	if err := ctx.Err(); err != nil {
		if !boundedSlice {
			c.invalid = true
			c.fail("cancelled")
		}
		return err
	}
	return nil
}

func (c *Catalog) checkOperation(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		c.invalid = true
		c.fail("cancelled")
		return err
	}
	if c.counters.CheckOperations >= c.limits.CheckOperations {
		c.invalid = true
		c.fail("check_budget")
		return agentapi.Wrap(agentapi.Limit, errors.New("catalog revalidation budget exhausted; renew the epoch"))
	}
	c.counters.CheckOperations++
	return nil
}

// Files supplies independently confined native store openers. It never broadens
// authority to a common ancestor when a dependency resides in another home.
func (c *Catalog) Files() transcriptio.Opener {
	c.initializeAuthority()
	return approvedFiles{roots: slices.Clone(c.authorities)}
}

type authority struct {
	counters *Counters
	home     string
	root     string
	info     fs.FileInfo
}

type approvedFiles struct{ roots []authority }

func (c *Catalog) initializeAuthority() {
	if c.authorityReady {
		return
	}
	c.authorityReady = true
	if len(c.homes) > c.limits.Roots {
		return
	}
	for _, home := range c.homes {
		if !filepath.IsAbs(home) || len(home) > 8192 {
			c.authorityFailures++
			continue
		}
		measureResolution(&c.counters)
		root, err := filepath.EvalSymlinks(home)
		if err != nil {
			c.authorityFailures++
			continue
		}
		measureStat(&c.counters)
		info, err := os.Lstat(root)
		if err != nil || !info.IsDir() {
			c.authorityFailures++
			continue
		}
		c.authorities = append(c.authorities, authority{home: home, root: root, info: info, counters: &c.counters})
	}
}

func (a approvedFiles) opener(path string) (authority, string, error) {
	if !filepath.IsAbs(path) {
		return authority{}, "", fs.ErrPermission
	}
	for _, approved := range a.roots {
		home, root := approved.home, approved.root
		measureResolution(approved.counters)
		resolved, err := filepath.EvalSymlinks(home)
		if err != nil || resolved != root {
			continue
		}
		measureStat(approved.counters)
		info, err := os.Lstat(root)
		if err != nil || !os.SameFile(approved.info, info) || !info.IsDir() {
			continue
		}
		for _, spelling := range []string{filepath.Clean(home), root} {
			for _, store := range []string{"sessions", "archived_sessions"} {
				if !local.PathWithin(path, filepath.Join(spelling, store)) {
					continue
				}
				relative, err := filepath.Rel(spelling, path)
				if err != nil {
					continue
				}
				expected := filepath.Join(root, relative)
				measureResolution(approved.counters)
				canonical, err := filepath.EvalSymlinks(path)
				if err == nil && canonical == expected {
					return approved, canonical, nil
				}
			}
		}
	}
	return authority{}, "", fs.ErrPermission
}

func (a approvedFiles) Lstat(path string) (fs.FileInfo, error) {
	o, p, e := a.opener(path)
	if e != nil {
		return nil, e
	}
	return measuredLstat(o.counters, o.root, p)
}

func (a approvedFiles) EvalSymlinks(path string) (string, error) {
	_, p, e := a.opener(path)
	return p, e
}

func (a approvedFiles) OpenRegular(path string) (transcriptio.File, error) {
	o, p, e := a.opener(path)
	if e != nil {
		return nil, e
	}
	f, err := openAuthorityRegular(o, p)
	if err != nil {
		return nil, err
	}
	return measuredFile{File: f, counters: o.counters}, nil
}

// openAuthorityRegular binds the opened root descriptor to its original directory
// identity before opening any data, so replacing an approved home cannot widen it.
func openAuthorityRegular(o authority, p string) (*os.File, error) {
	measureRootOpen(o.counters)
	opened, err := os.OpenRoot(o.root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = opened.Close() }()
	measureStat(o.counters)
	info, err := opened.Stat(".")
	if err != nil || !os.SameFile(o.info, info) {
		return nil, transcriptio.ErrChanged
	}
	relative, err := filepath.Rel(o.root, p)
	if err != nil {
		return nil, err
	}
	if relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, fs.ErrPermission
	}
	// Bind each directory descriptor without following component symlinks. A
	// canonical path check before this call cannot fence a later in-root retarget.
	parts := strings.Split(relative, string(filepath.Separator))
	parent := opened
	for _, part := range parts[:len(parts)-1] {
		measureStat(o.counters)
		before, err := parent.Lstat(part)
		if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			return nil, transcriptio.ErrChanged
		}
		measureRootOpen(o.counters)
		next, err := parent.OpenRoot(part)
		if err != nil {
			return nil, err
		}
		defer func() { _ = next.Close() }()
		measureStat(o.counters)
		after, err := next.Stat(".")
		measureStat(o.counters)
		named, namedErr := parent.Lstat(part)
		if err != nil || namedErr != nil || !sameDirectory(before, after) || !sameDirectory(before, named) {
			return nil, transcriptio.ErrChanged
		}
		parent = next
	}
	name := parts[len(parts)-1]
	measureStat(o.counters)
	before, err := parent.Lstat(name)
	if err != nil || !before.Mode().IsRegular() {
		return nil, transcriptio.ErrNotRegularFile
	}
	measureFileOpen(o.counters)
	f, err := parent.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	measureStat(o.counters)
	after, err := f.Stat()
	measureStat(o.counters)
	named, namedErr := parent.Lstat(name)
	if err != nil || namedErr != nil || !transcriptio.SameObservation(before, after) || !transcriptio.SameObservation(before, named) {
		_ = f.Close()
		return nil, transcriptio.ErrChanged
	}
	measureStat(o.counters)
	rootInfo, err := os.Lstat(o.root)
	home := o.home
	if home == "" {
		home = o.root
	}
	measureResolution(o.counters)
	resolved, resolveErr := filepath.EvalSymlinks(home)
	if err != nil || !rootInfo.IsDir() || !os.SameFile(o.info, rootInfo) || resolveErr != nil || resolved != o.root {
		_ = f.Close()
		return nil, transcriptio.ErrChanged
	}
	return f, nil
}
