package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// Delta is verified reconciliation between two complete identity roots. It
// carries changed/deleted summaries, never partial canonical LIST authority.
type Delta struct {
	Prior, Next ObjectRef
	Changed     []Row
	Removed     []string
	Rebuild     bool
}

// Delta validates prior and new immutable paths. Equal hashed subtrees are
// skipped. A structural split may read the changed subtree in full; this is
// reconciliation work, not an advertised bounded user query. An unavailable
// prior root requires a complete summary rebuild, preserving all results.
func (s *Snapshot) Delta(ctx context.Context, prior ObjectRef) (Delta, error) {
	d := Delta{Prior: prior, Next: s.head.Identity}
	if err := s.check(ctx); err != nil {
		return d, err
	}
	if prior == d.Next {
		return d, nil
	}
	old := map[string]CatalogEntry{}
	next := map[string]CatalogEntry{}
	var collect func(ObjectRef, map[string]CatalogEntry, int) error
	collect = func(ref ObjectRef, target map[string]CatalogEntry, depth int) error {
		if depth >= maxDepth {
			return errors.New("catalog delta depth exceeded")
		}
		n, err := s.readNode(ctx, ref)
		if err != nil {
			return err
		}
		for _, child := range n.Children {
			if err = collect(child.Ref, target, depth+1); err != nil {
				return err
			}
		}
		for _, leaf := range n.Leaves {
			var r record
			if err = json.Unmarshal(leaf.Value, &r); err != nil {
				return err
			}
			if r.Entry != nil {
				if leafSessionKey(r.Entry) != leaf.Key || r.Entry.Revision == "" {
					return errors.New("catalog delta identity mismatch")
				}
				target[leaf.Key] = *r.Entry
			}
		}
		return nil
	}
	var diff func(ObjectRef, ObjectRef, int) error
	diff = func(a, b ObjectRef, depth int) error {
		if a == b {
			return nil
		}
		if depth >= maxDepth {
			return errors.New("catalog delta depth exceeded")
		}
		na, err := s.readNode(ctx, a)
		if err != nil {
			return err
		}
		nb, err := s.readNode(ctx, b)
		if err != nil {
			return err
		}
		aligned := len(na.Children) > 0 && len(na.Children) == len(nb.Children)
		if aligned {
			for i := range na.Children {
				if na.Children[i].Max != nb.Children[i].Max {
					aligned = false
					break
				}
			}
		}
		if aligned {
			for i := range na.Children {
				if err = diff(na.Children[i].Ref, nb.Children[i].Ref, depth+1); err != nil {
					return err
				}
			}
			return nil
		}
		if err = collect(a, old, depth); err != nil {
			return err
		}
		return collect(b, next, depth)
	}
	if prior.Key == "" {
		d.Rebuild = true
		if err := collect(d.Next, next, 0); err != nil {
			return d, err
		}
	} else if err := diff(prior, d.Next, 0); err != nil {
		// Unknown previous roots can be collected after the lifetime. Integrity,
		// cancellation and network errors fail closed rather than hiding corruption.
		if !errors.Is(err, storage.ErrNotFound) {
			return d, err
		}
		d.Rebuild = true
		old = map[string]CatalogEntry{}
		next = map[string]CatalogEntry{}
		if err = collect(d.Next, next, 0); err != nil {
			return d, err
		}
	}
	for key, entry := range next {
		if previous, ok := old[key]; !ok || previous.Revision != entry.Revision || previous.Metadata != entry.Metadata {
			d.Changed = append(d.Changed, Row{key, entry})
		}
	}
	for key := range old {
		if _, ok := next[key]; !ok {
			d.Removed = append(d.Removed, key)
		}
	}
	return d, ctx.Err()
}
