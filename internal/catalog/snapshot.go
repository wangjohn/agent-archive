package catalog

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/storage"
)

// ErrStaleCursor requires a new snapshot and a restarted query.
var ErrStaleCursor = errors.New("catalog snapshot changed or expired; refresh the view")

// Index selects one lexicographically ordered catalog tree.
type Index string

// Indexed ranges avoid scanning unrelated tree paths. Identity ranges include
// tombstones; use Find for a single live identity. Other indexes omit tombstones.
const (
	IdentityIndex Index = "identity"
	CaptureIndex  Index = "capture"
	ActivityIndex Index = "activity"
	ProjectIndex  Index = "project"
)

// Query is an indexed range, with inclusive Lower and exclusive Upper bounds.
// No predicate is silently post-filtered: text, parent/root, replay and combined
// filters belong to a complete summary fallback above this API.
type Query struct {
	Index        Index
	Lower, Upper string
	Reverse      bool
}

// Row identifies an immutable metadata revision selected from an index.
type Row struct {
	Key   string
	Entry CatalogEntry
}

// Page contains at most the requested number of live entries.
type Page struct {
	Rows []Row
	Next string
}

// NodeCache is a bounded in-memory cache of hash-validated immutable bytes.
// It stores no transcripts and never caches mutable head existence authority.
// A zero value is ready for use; at most 256 bounded nodes are retained.
type NodeCache struct {
	mu    sync.Mutex
	bytes map[ObjectRef][]byte
}

// Snapshot pins the four roots for at most ten minutes from request start.
// Cursors can only continue this snapshot. Reopening, expiry or a head change
// rejects them, including across process restart, rather than extending a TTL.
type Snapshot struct {
	writer    *Writer
	head      CatalogHead
	etag      string
	started   time.Time
	nonce     string
	cache     *NodeCache
	requestMu sync.Mutex
	requests  uint64
}

// OpenSnapshot reads one fresh qualified versioned head. Time spent opening is
// part of the monotonic lifetime, so a stalled request cannot extend retention.
func OpenSnapshot(ctx context.Context, store storage.ObjectStore, cache *NodeCache) (*Snapshot, error) {
	return openSnapshot(ctx, store, cache, true)
}

func openSnapshot(ctx context.Context, store storage.ObjectStore, cache *NodeCache, active bool) (*Snapshot, error) {
	return openSnapshotStarted(ctx, store, cache, active, time.Now())
}

func openSnapshotStarted(ctx context.Context, store storage.ObjectStore, cache *NodeCache, active bool, started time.Time) (*Snapshot, error) {
	w, err := New(store)
	if err != nil {
		return nil, err
	}
	if active {
		if err = w.Coordinator().Active(ctx); err != nil {
			return nil, err
		}
	}
	h, etag, err := w.Head(ctx)
	if err != nil {
		return nil, err
	}
	if etag == "" {
		return nil, storage.ErrNotFound
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if time.Since(started) >= SnapshotLifetime {
		return nil, ErrStaleCursor
	}
	nonce, err := NewMutationID()
	if err != nil {
		return nil, err
	}
	if cache == nil {
		cache = &NodeCache{}
	}
	return &Snapshot{writer: w, head: h, etag: etag, started: started, nonce: nonce, cache: cache}, nil
}

// Root identifies the complete identity universe, independently from GC lease
// epochs. It is suitable for verified prior-root to new-root reconciliation.
func (s *Snapshot) Root() ObjectRef { return s.head.Identity }

func (s *Snapshot) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if time.Since(s.started) >= SnapshotLifetime {
		return ErrStaleCursor
	}
	return nil
}

func (s *Snapshot) freshRequest(ctx context.Context) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	s.requestMu.Lock()
	first := s.requests == 0
	s.requests++
	s.requestMu.Unlock()
	// Opening already supplied this request's fresh head. Subsequent Query/Find
	// requests obtain exactly one new same-response version observation.
	if first {
		return nil
	}
	_, etag, err := s.writer.Head(ctx)
	if err != nil {
		return err
	}
	if etag != s.etag {
		return ErrStaleCursor
	}
	return s.check(ctx)
}

func (s *Snapshot) root(q Query) (ObjectRef, error) {
	switch q.Index {
	case IdentityIndex:
		return s.head.Identity, nil
	case CaptureIndex:
		return s.head.Capture, nil
	case ActivityIndex:
		return s.head.Activity, nil
	case ProjectIndex:
		return s.head.Project, nil
	default:
		return ObjectRef{}, errors.New("unsupported catalog index")
	}
}

func (s *Snapshot) readNode(ctx context.Context, ref ObjectRef) (node, error) {
	if err := ref.validate(); err != nil {
		return node{}, err
	}
	if err := ctx.Err(); err != nil {
		return node{}, err
	}
	if time.Since(s.started) >= SnapshotLifetime {
		return node{}, ErrStaleCursor
	}
	if ref.Key == "" {
		return node{}, nil
	}
	s.cache.mu.Lock()
	raw := append([]byte(nil), s.cache.bytes[ref]...)
	s.cache.mu.Unlock()
	if len(raw) == 0 {
		var err error
		raw, err = s.writer.readRef(ctx, ref, maxNodeBytes)
		if err != nil {
			return node{}, err
		}
	} else if !storage.VerifySHA256(raw, ref.SHA256) {
		return node{}, storage.ErrChecksumMismatch
	}
	if err := s.check(ctx); err != nil {
		return node{}, err
	}
	n, err := decodeNode(raw)
	if err != nil {
		return node{}, err
	}
	s.cache.mu.Lock()
	if s.cache.bytes == nil || len(s.cache.bytes) >= 256 {
		s.cache.bytes = make(map[ObjectRef][]byte)
	}
	s.cache.bytes[ref] = append([]byte(nil), raw...)
	s.cache.mu.Unlock()
	return n, nil
}

type snapshotCursor struct {
	Nonce string
	Root  ObjectRef
	Query Query
	After string
	Limit int
}

// Query traverses only paths intersecting the selected range. With the three
// order indexes it uses O(log N + results) node work, excluding metadata bodies.
// Identity ranges have additional tombstone work; exact Find avoids that cost.
// Every continuation rereads the head and validates its root/query binding.
func (s *Snapshot) Query(ctx context.Context, q Query, cursor string, limit int) (Page, error) {
	q = defaultRange(q)
	if limit <= 0 || limit > 10000 {
		return Page{}, errors.New("catalog page limit must be 1..10000")
	}
	root, err := s.root(q)
	if err != nil {
		return Page{}, err
	}
	if q.Upper != "" && q.Lower > q.Upper {
		return Page{}, errors.New("invalid catalog range")
	}
	if err = s.freshRequest(ctx); err != nil {
		return Page{}, err
	}
	token := snapshotCursor{Nonce: s.nonce, Root: root, Query: q, Limit: limit}
	if cursor != "" {
		raw, e := base64.RawURLEncoding.DecodeString(cursor)
		var prior snapshotCursor
		if e != nil || len(raw) > 8192 || json.Unmarshal(raw, &prior) != nil || prior.Nonce != token.Nonce || prior.Root != root || prior.Query != q || prior.Limit != limit || prior.After == "" {
			return Page{}, ErrStaleCursor
		}
		token.After = prior.After
	}
	page := Page{}
	last := ""
	var visit func(ObjectRef, int) error
	visit = func(ref ObjectRef, depth int) error {
		if depth >= maxDepth {
			return errors.New("catalog tree depth exceeded")
		}
		n, e := s.readNode(ctx, ref)
		if e != nil {
			return e
		}
		for j := range len(n.Children) {
			i := j
			if q.Reverse {
				i = len(n.Children) - 1 - j
			}
			c := n.Children[i]
			lower := ""
			if i > 0 {
				lower = n.Children[i-1].Max
			}
			if c.Max < q.Lower || q.Upper != "" && lower >= q.Upper || token.After != "" && ((!q.Reverse && c.Max <= token.After) || (q.Reverse && lower >= token.After)) {
				continue
			}
			if e = visit(c.Ref, depth+1); e != nil {
				return e
			}
			if len(page.Rows) > limit {
				return nil
			}
		}
		for j := range len(n.Leaves) {
			i := j
			if q.Reverse {
				i = len(n.Leaves) - 1 - j
			}
			leaf := n.Leaves[i]
			if leaf.Key < q.Lower || q.Upper != "" && leaf.Key >= q.Upper || token.After != "" && ((!q.Reverse && leaf.Key <= token.After) || (q.Reverse && leaf.Key >= token.After)) {
				continue
			}
			var entry *CatalogEntry
			if q.Index == IdentityIndex {
				var r record
				if e = json.Unmarshal(leaf.Value, &r); e != nil {
					return e
				}
				entry = r.Entry
			} else {
				entry = &CatalogEntry{}
				if e = json.Unmarshal(leaf.Value, entry); e != nil {
					return e
				}
			}
			if entry == nil {
				continue
			}
			key := leafSessionKey(entry)
			if key == "" || entry.Revision == "" || entry.Metadata.Key == "" || entry.Metadata.SHA256 == "" {
				return errors.New("invalid catalog entry")
			}
			if q.Index == IdentityIndex && key != leaf.Key {
				return errors.New("catalog identity mismatch")
			}
			if q.Index != IdentityIndex {
				keys := allOrderKeys(key, entry)
				idx := map[Index]int{CaptureIndex: 0, ActivityIndex: 1, ProjectIndex: 2}[q.Index]
				valid := false
				for _, key := range keys[idx] {
					if key == leaf.Key {
						valid = true
					}
				}
				if !valid {
					return errors.New("catalog order key mismatch")
				}
			}
			page.Rows = append(page.Rows, Row{Key: key, Entry: *entry})
			if len(page.Rows) > limit {
				return nil
			}
			last = leaf.Key
		}
		return nil
	}
	if err = visit(root, 0); err != nil {
		return Page{}, err
	}
	if len(page.Rows) > limit {
		page.Rows = page.Rows[:limit]
		token.After = last
		raw, e := json.Marshal(token)
		if e != nil {
			return Page{}, e
		}
		page.Next = base64.RawURLEncoding.EncodeToString(raw)
	}
	if time.Since(s.started) >= SnapshotLifetime {
		return Page{}, ErrStaleCursor
	}
	if err = ctx.Err(); err != nil {
		return Page{}, err
	}
	return page, nil
}

// Find resolves one canonical identity in O(log N) tree work.
func (s *Snapshot) Find(ctx context.Context, key string) (*CatalogEntry, error) {
	if err := s.freshRequest(ctx); err != nil {
		return nil, err
	}
	ref := s.head.Identity
	for range maxDepth {
		n, err := s.readNode(ctx, ref)
		if err != nil {
			return nil, err
		}
		if len(n.Children) == 0 {
			for _, leaf := range n.Leaves {
				if leaf.Key != key {
					continue
				}
				var r record
				if err = json.Unmarshal(leaf.Value, &r); err != nil {
					return nil, err
				}
				if r.Entry != nil && (leafSessionKey(r.Entry) != key || r.Entry.Revision == "") {
					return nil, errors.New("catalog identity mismatch")
				}
				return r.Entry, nil
			}
			return nil, nil
		}
		found := false
		for _, child := range n.Children {
			if child.Max >= key {
				ref = child.Ref
				found = true
				break
			}
		}
		if !found {
			return nil, nil
		}
	}
	return nil, errors.New("catalog tree depth exceeded")
}

// ReadMetadata verifies the immutable body selected by this snapshot. It never
// substitutes a newer canonical revision after selection.
func (s *Snapshot) ReadMetadata(ctx context.Context, entry CatalogEntry) ([]byte, error) {
	if time.Since(s.started) >= SnapshotLifetime {
		return nil, ErrStaleCursor
	}
	raw, err := s.writer.readRef(ctx, entry.Metadata, 32<<20)
	if err != nil {
		return nil, err
	}
	if err = s.check(ctx); err != nil {
		return nil, err
	}
	return raw, nil
}

// Count returns exact index entry counts using validated subtree aggregates.
// It does not count live identities through tombstones; choose an order range.
func (s *Snapshot) Count(ctx context.Context, q Query) (uint64, error) {
	q = defaultRange(q)
	if q.Index == IdentityIndex {
		return 0, errors.New("identity count includes tombstones")
	}
	root, err := s.root(q)
	if err != nil {
		return 0, err
	}
	if err = s.check(ctx); err != nil {
		return 0, err
	}
	var visit func(ObjectRef, string, string, int) (uint64, error)
	visit = func(ref ObjectRef, lower, upper string, depth int) (uint64, error) {
		if depth >= maxDepth {
			return 0, errors.New("catalog depth exceeded")
		}
		if upper != "" && upper < q.Lower || q.Upper != "" && lower >= q.Upper {
			return 0, nil
		}
		n, e := s.readNode(ctx, ref)
		if e != nil {
			return 0, e
		}
		if lower >= q.Lower && upper != "" && (q.Upper == "" || upper < q.Upper) {
			return n.Count, nil
		}
		var total uint64
		for i, c := range n.Children {
			lo := lower
			if i > 0 {
				lo = n.Children[i-1].Max
			}
			count, e := visit(c.Ref, lo, c.Max, depth+1)
			if e != nil {
				return 0, e
			}
			if count > ^uint64(0)-total {
				return 0, errors.New("catalog count overflow")
			}
			total += count
		}
		for _, leaf := range n.Leaves {
			if leaf.Key >= q.Lower && (q.Upper == "" || leaf.Key < q.Upper) {
				total++
			}
		}
		return total, nil
	}
	count, err := visit(root, "", "", 0)
	if err != nil {
		return 0, err
	}
	if err = s.check(ctx); err != nil {
		return 0, err
	}
	return count, nil
}

func defaultRange(q Query) Query {
	if q.Lower == "" && q.Upper == "" {
		switch q.Index {
		case CaptureIndex, ActivityIndex:
			q.Lower = "0"
			q.Upper = ":"
		case ProjectIndex:
			q.Lower = "project/"
			q.Upper = "project0"
		}
	}
	return q
}
