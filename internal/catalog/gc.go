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
	if _, recovery := barrier.(completedCoordinator); recovery {
		return errors.New("completed recovery barrier cannot collect")
	}
	if _, recovery := barrier.(recoveryCoordinator); recovery {
		return errors.New("recovery barrier cannot collect")
	}
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
	owned, ownBarrier := barrier.(heldCoordinator)
	if !ownBarrier {
		defer release()
	}
	clock, err := w.clock.CatalogServerClock(ctx)
	if err != nil {
		return err
	}
	if err = validateClock(clock); err != nil {
		return err
	}
	// Even a caller-supplied history inventory cannot replace protocol9's real
	// destination-wide admission fence. All source/commit owners must drain.
	coordinator := w.Coordinator()
	var ownRelease func()
	if ownBarrier {
		if owned.coordinator.writer.store != w.store {
			return errors.New("catalog barrier belongs to another destination")
		}
		ownRelease = release
	} else {
		seal, e := coordinator.Seal(ctx)
		if e != nil {
			return e
		}
		ownProtected, heldRelease, e := coordinator.HeldBarrier(seal).Hold(ctx)
		if e != nil {
			return e
		}
		protected = append(protected, ownProtected...)
		ownRelease = heldRelease
	}
	// On failure, retain the durable seal/hold alongside the GC head lease.
	// Recovery requires the exact observed GC owner and fresh global barrier.
	h, etag, err := w.gcHead(ctx)
	if err != nil {
		return err
	}
	h, leaseETag, err := w.acquireGC(ctx, h, etag, clock, protected)
	if err != nil {
		return err
	}
	// Any incomplete inventory retains the durable lease. It never expires
	// into permission and requires explicit owner recovery under the barrier.
	live := map[string]bool{HeadKey: true, CoordinatorKey: true}
	// Keep traversal evidence separate from protected membership, and share it
	// across snapshots so immutable subtrees are verified only once per GC.
	visited := make(map[string]bool)
	if err = w.markProtected(ctx, protected, live); err != nil {
		return err
	}
	if err = w.markHead(ctx, h, live, visited); err != nil {
		return err
	}
	if err = w.markRetained(ctx, h, clock, live, visited); err != nil {
		return err
	}
	if err = w.verifyGCSweep(ctx, h, leaseETag); err != nil {
		return err
	}
	if err = w.removeUnreachable(ctx, live, h, leaseETag); err != nil {
		return err
	}
	if err = w.releaseGC(ctx, h, leaseETag); err != nil {
		return err
	}
	_ = ownRelease // linked release already settled this exact durable hold.
	return nil
}

func (w *Writer) acquireGC(ctx context.Context, h CatalogHead, etag string, clock storage.CatalogTime, inventory []ObjectRef) (CatalogHead, string, error) {
	if h.GCLease != "" {
		return h, "", errors.New("catalog GC lease requires explicit recovery")
	}
	owner, err := NewMutationID()
	if err != nil {
		return h, "", err
	}
	prior := h
	h.GCLease = owner
	h.Epoch = owner
	h.Generation++
	if err = w.bindGCLease(ctx, prior, etag, &h, inventory); err != nil {
		return h, "", err
	}
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

func (w *Writer) markRetained(ctx context.Context, h CatalogHead, clock storage.CatalogTime, live, visited map[string]bool) error {
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
		if err = w.markHead(ctx, old, live, visited); err != nil {
			return err
		}
		previous = old.Previous
		successorTime = old.PublicationWitness.LastModified.Add(old.PublicationWitness.Precision)
	}
	return nil
}

func (w *Writer) removeUnreachable(ctx context.Context, live map[string]bool, h CatalogHead, etag string) error {
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
		if !live[obj.Key] && !coordinatorObject(obj.Key) {
			if err := w.verifyGCSweep(ctx, h, etag); err != nil {
				return err
			}
			if err := w.store.Delete(ctx, obj.Key); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *Writer) releaseGC(ctx context.Context, h CatalogHead, etag string) error {
	owner := h.GCLease
	raw, err := w.bindGCRelease(ctx, h, owner)
	if err != nil {
		return err
	}
	_, err = w.conditional.PutConditional(ctx, HeadKey, raw, storage.PutCondition{MatchETag: etag})
	if err != nil {
		return errors.Join(ErrCommitUnknown, err)
	}
	return w.finishGCRelease(ctx, owner)
}

func (w *Writer) markHead(ctx context.Context, h CatalogHead, live, visited map[string]bool) error {
	if err := h.validateReferences(); err != nil {
		return err
	}
	for _, root := range []ObjectRef{h.Identity, h.Capture, h.Activity, h.Project, h.Receipts} {
		if err := w.markTree(ctx, root, live, visited, 0); err != nil {
			return err
		}
	}
	return nil
}

func (w *Writer) markTree(ctx context.Context, ref ObjectRef, live, visited map[string]bool, depth int) error {
	if ref.Key == "" || visited[ref.Key] {
		return nil
	}
	if depth >= maxDepth {
		return errors.New("GC tree depth exceeded")
	}
	live[ref.Key] = true
	visited[ref.Key] = true
	n, err := w.readNode(ctx, ref)
	if err != nil {
		return err
	}
	for _, child := range n.Children {
		if err = w.markTree(ctx, child.Ref, live, visited, depth+1); err != nil {
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
	return w.recoverLinkedGC(ctx, owner)
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
	h = CatalogHead{Schema: 4, Protocol: 9, Generation: 1, Epoch: id, PublicationEpoch: id}
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
