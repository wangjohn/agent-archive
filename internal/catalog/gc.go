package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// Barrier must drain and hold ALL destination writers, history preservation,
// and pending transactions, across machines, until release. Its inventory is
// globally complete, not a local state scan. An unavailable witness must error.
// Migration owns the production coordinator; a process-local mutex is no proof.
type Barrier interface {
	Hold(context.Context) (protected []ObjectRef, release func(), err error)
}

// Collect holds both a global writer barrier and a durable head lease. A crash
// leaves the lease closed; only RecoverGC under the same global barrier can
// release it. GC never interprets timeout or age as lease ownership.
func (w *Writer) Collect(ctx context.Context, barrier Barrier) error {
	if barrier == nil {
		return errors.New("complete writer barrier is required")
	}
	protected, release, err := barrier.Hold(ctx)
	if err != nil {
		return err
	}
	if release == nil {
		return errors.New("writer barrier did not provide held ownership")
	}
	defer release()
	h, etag, err := w.gcHead(ctx)
	if err != nil {
		return err
	}
	clock, err := w.clock.CatalogServerClock(ctx)
	if err != nil {
		return err
	}
	if err = validateClock(clock); err != nil {
		return err
	}
	h, leaseETag, err := w.acquireGC(ctx, h, etag, clock)
	if err != nil {
		return err
	}
	// Any incomplete inventory retains the durable lease. It never expires
	// into permission and requires explicit owner recovery under the barrier.
	live := map[string]bool{HeadKey: true}
	if err = w.markProtected(ctx, protected, live); err != nil {
		return err
	}
	if err = w.markHead(ctx, h, live); err != nil {
		return err
	}
	if err = w.markRetained(ctx, h, clock, live); err != nil {
		return err
	}
	if err = w.removeUnreachable(ctx, live); err != nil {
		return err
	}
	return w.releaseGC(ctx, h, leaseETag)
}

func (w *Writer) acquireGC(ctx context.Context, h CatalogHead, etag string, clock storage.CatalogTime) (CatalogHead, string, error) {
	if h.GCLease != "" {
		return h, "", errors.New("catalog GC lease requires explicit recovery")
	}
	owner, err := NewMutationID()
	if err != nil {
		return h, "", err
	}
	h.GCLease = owner
	h.Epoch = owner
	h.Generation++
	raw, err := json.Marshal(h)
	if err != nil {
		return h, "", err
	}
	leaseETag, err := w.conditional.PutConditional(ctx, HeadKey, raw, storage.PutCondition{MatchETag: etag})
	if err != nil {
		return h, "", err
	}
	if leaseETag == "" {
		return h, "", errors.New("GC lease acknowledgement lacks validator")
	}
	leased, version, err := w.readHead(ctx)
	if err != nil {
		return h, "", err
	}
	if leased.GCLease != owner || version.ETag != leaseETag {
		return h, "", errors.New("GC lease version differs from acknowledgement")
	}
	after, err := w.clock.CatalogServerClock(ctx)
	if err != nil {
		return h, "", err
	}
	if err = validateClock(after); err != nil {
		return h, "", err
	}
	if version.LastModified.Add(version.Precision).Before(clock.Earliest) || version.LastModified.After(after.Latest) {
		return h, "", errors.New("provider lease timestamp and qualified clock differ")
	}
	return leased, leaseETag, nil
}

func (w *Writer) markProtected(ctx context.Context, protected []ObjectRef, live map[string]bool) error {
	for _, ref := range protected {
		if ref.Key == "" || ref.SHA256 == "" {
			return errors.New("incomplete barrier reference")
		}
		if _, err := w.readRef(ctx, ref, 64<<20); err != nil {
			return err
		}
		live[ref.Key] = true
	}
	return nil
}

func (w *Writer) markRetained(ctx context.Context, h CatalogHead, clock storage.CatalogTime, live map[string]bool) error {
	if h.PublicationWitness.LastModified.After(clock.Latest) {
		return errors.New("catalog provider publication time is ahead of qualified clock")
	}
	// Each older root retires at its SUCCESSOR's provider commit witness, plus
	// timestamp precision. Lease rewrites never reset original publication age.
	successorTime := h.PublicationWitness.LastModified.Add(h.PublicationWitness.Precision)
	previous := h.Previous
	for previous.Key != "" && !successorTime.Before(clock.Earliest.Add(-SnapshotLifetime)) {
		live[previous.Key] = true
		b, err := w.readRef(ctx, previous, 16<<10)
		if err != nil {
			return err
		}
		var old CatalogHead
		if err = json.Unmarshal(b, &old); err != nil {
			return err
		}
		v := storage.CatalogObjectVersion{ETag: old.PublicationWitness.ETag, LastModified: old.PublicationWitness.LastModified, Precision: old.PublicationWitness.Precision}
		if err = old.observeVersion(v); err != nil {
			return err
		}
		if old.Schema != 4 || old.PublicationWitness.LastModified.After(successorTime) {
			return errors.New("invalid retained head or regressed provider clock")
		}
		if err = w.markHead(ctx, old, live); err != nil {
			return err
		}
		previous = old.Previous
		successorTime = old.PublicationWitness.LastModified.Add(old.PublicationWitness.Precision)
	}
	return nil
}

func (w *Writer) removeUnreachable(ctx context.Context, live map[string]bool) error {
	var candidates []storage.Object
	for _, prefix := range []string{"catalog-v4/", "sessions/"} {
		objects, err := w.store.List(ctx, prefix)
		if err != nil {
			return err
		}
		for _, obj := range objects {
			if metadataKey(obj.Key) {
				return errors.New("mixed legacy metadata prevents catalog collection")
			}
		}
		candidates = append(candidates, objects...)
	}
	for _, obj := range candidates {
		if !live[obj.Key] {
			if err := w.store.Delete(ctx, obj.Key); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *Writer) releaseGC(ctx context.Context, h CatalogHead, etag string) error {
	h.GCLease = ""
	id, err := NewMutationID()
	if err != nil {
		return err
	}
	h.Epoch = id
	h.Generation++
	raw, err := json.Marshal(h)
	if err != nil {
		return err
	}
	_, err = w.conditional.PutConditional(ctx, HeadKey, raw, storage.PutCondition{MatchETag: etag})
	return err
}

func (w *Writer) markHead(ctx context.Context, h CatalogHead, live map[string]bool) error {
	for _, root := range []ObjectRef{h.Identity, h.Capture, h.Activity, h.Project, h.Receipts} {
		if err := w.markTree(ctx, root, live, 0); err != nil {
			return err
		}
	}
	return nil
}

func (w *Writer) markTree(ctx context.Context, ref ObjectRef, live map[string]bool, depth int) error {
	if ref.Key == "" || live[ref.Key] {
		return nil
	}
	if depth >= maxDepth {
		return errors.New("GC tree depth exceeded")
	}
	live[ref.Key] = true
	n, err := w.readNode(ctx, ref)
	if err != nil {
		return err
	}
	for _, child := range n.Children {
		if err = w.markTree(ctx, child.Ref, live, depth+1); err != nil {
			return err
		}
	}
	for _, leaf := range n.Leaves {
		var r record
		if err = json.Unmarshal(leaf.Value, &r); err != nil {
			return err
		}
		entry := r.Entry
		// Order leaves carry entries directly; receipt leaves carry no metadata.
		if entry == nil {
			var direct CatalogEntry
			if err = json.Unmarshal(leaf.Value, &direct); err != nil {
				return err
			}
			if direct.Metadata.Key != "" {
				entry = &direct
			}
		}
		if entry != nil {
			if err = w.verifyEntry(ctx, leafSessionKey(entry), entry); err != nil {
				return err
			}
			live[entry.Metadata.Key] = true
			refs, e := entry.Summary.SourceReferences()
			if e != nil {
				return e
			}
			for _, source := range refs {
				live[source.Key] = true
			}
		}
	}
	return nil
}

func leafSessionKey(e *CatalogEntry) string {
	key, _ := archive.MetadataObjectKey(e.Summary.Harness.Name, e.Summary.SessionID)
	return key
}

// RecoverGC requires the exact observed owner and a freshly held global
// barrier. It only releases the fence; a subsequent Collect rebuilds inventory.
func (w *Writer) RecoverGC(ctx context.Context, barrier Barrier, owner string) error {
	if barrier == nil || owner == "" {
		return errors.New("GC recovery ownership required")
	}
	_, release, err := barrier.Hold(ctx)
	if err != nil {
		return err
	}
	if release == nil {
		return errors.New("writer barrier not held")
	}
	defer release()
	h, etag, err := w.Head(ctx)
	if err != nil {
		return err
	}
	if h.GCLease != owner {
		return ErrConflict
	}
	return w.releaseGC(ctx, h, etag)
}

func (w *Writer) gcHead(ctx context.Context) (CatalogHead, string, error) {
	h, etag, err := w.Head(ctx)
	if err != nil || etag != "" {
		return h, etag, err
	}
	id, err := NewMutationID()
	if err != nil {
		return h, "", err
	}
	h = CatalogHead{Schema: 4, Generation: 1, Epoch: id, PublicationEpoch: id}
	raw, err := json.Marshal(h)
	if err != nil {
		return h, "", err
	}
	if _, err = w.conditional.PutConditional(ctx, HeadKey, raw, storage.PutCondition{CreateOnly: true}); err != nil {
		return h, "", err
	}
	return w.Head(ctx)
}

func validateClock(clock storage.CatalogTime) error {
	if clock.Earliest.IsZero() || clock.Latest.Before(clock.Earliest) || clock.Latest.Sub(clock.Earliest) > time.Minute {
		return errors.New("catalog provider clock uncertainty is unqualified")
	}
	return nil
}
