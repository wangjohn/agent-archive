// Package catalog implements the opt-in immutable catalog writer protocol.
package catalog

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// HeadKey is the unique mutable catalog commit point.
const HeadKey = "catalog-v4/head.json"

// SnapshotLifetime bounds the validity of a pinned catalog root.
const SnapshotLifetime = 10 * time.Minute

// ErrConflict rejects a mutation against a changed session revision.
var ErrConflict = errors.New("catalog session revision conflict")

// ErrMutationReuse rejects reuse of an ID for different frozen input.
var ErrMutationReuse = errors.New("catalog mutation ID reused")

// ErrCommitUnknown retains the transaction until fresh authority resolves it.
var ErrCommitUnknown = errors.New("catalog commit acknowledgement unknown")

// ImmutableKind selects the content-addressed object namespace.
type ImmutableKind string

// Supported immutable catalog object kinds.
const (
	KindNodes    ImmutableKind = "nodes"
	KindMetadata ImmutableKind = "metadata"
	KindHeads    ImmutableKind = "heads"
)

// ObjectRef identifies exact immutable bytes by key and SHA256.
type ObjectRef struct {
	Key    string `json:"Key"`
	SHA256 string `json:"SHA256"`
}

// CatalogHead atomically commits all indexes and mutation receipts.
//
//revive:disable-next-line:exported -- Keep the accepted protocol API name.
type CatalogHead struct {
	Schema     uint64    `json:"Schema"`
	Generation uint64    `json:"Generation"`
	Epoch      string    `json:"Epoch"`
	Identity   ObjectRef `json:"Identity"`
	Capture    ObjectRef `json:"Capture"`
	Activity   ObjectRef `json:"Activity"`
	Project    ObjectRef `json:"Project"`
	// Receipts are committed with the roots, so a retry can prove its prior
	// commit even when a newer publication replaced the same session.
	Receipts           ObjectRef   `json:"Receipts"`
	Previous           ObjectRef   `json:"Previous"`
	CommittedAt        time.Time   `json:"CommittedAt"`
	GCLease            string      `json:"GCLease"`
	PublicationEpoch   string      `json:"PublicationEpoch"`
	PublicationWitness HeadWitness `json:"PublicationWitness"`
	PredecessorWitness HeadWitness `json:"PredecessorWitness"`
}

// HeadWitness preserves the original provider publication version through leases.
type HeadWitness struct {
	ETag         string        `json:"ETag"`
	LastModified time.Time     `json:"LastModified"`
	Precision    time.Duration `json:"Precision"`
	Epoch        string        `json:"Epoch"`
	RootsSHA256  string        `json:"RootsSHA256"`
}

// CatalogEntry holds immutable metadata and typed query summary fields.
//
//revive:disable-next-line:exported -- Keep the accepted protocol API name.
type CatalogEntry struct {
	Revision string           `json:"Revision"`
	Metadata ObjectRef        `json:"Metadata"`
	Summary  archive.Metadata `json:"Summary"`
}

// CatalogMutation replaces or deletes one expected session revision.
//
//revive:disable-next-line:exported -- Keep the accepted protocol API name.
type CatalogMutation struct {
	ID               string        `json:"ID"`
	SessionKey       string        `json:"SessionKey"`
	ExpectedRevision string        `json:"ExpectedRevision"`
	Next             *CatalogEntry `json:"Next"`
}

type record struct {
	Revision string        `json:"Revision"`
	Entry    *CatalogEntry `json:"Entry"`
}

type receipt struct {
	Digest         string `json:"Digest"`
	Revision       string `json:"Revision"`
	SessionKey     string `json:"SessionKey"`
	MetadataSHA256 string `json:"MetadataSHA256"`
}

// Writer publishes immutable catalog objects through a qualified store.
type Writer struct {
	store       storage.ObjectStore
	bounded     storage.LimitedGetter
	versioned   storage.CatalogVersionedGetter
	clock       storage.CatalogClock
	conditional storage.ConditionalPutter
}

// New requires qualified atomic writes and bounded versioned reads.
func New(store storage.ObjectStore) (*Writer, error) {
	qualified, ok := store.(storage.AtomicCatalogProvider)
	if !ok {
		return nil, storage.ErrAtomicCatalogUnqualified
	}
	if err := qualified.CatalogAtomicQualification(); err != nil {
		return nil, err
	}
	bounded, ok := store.(storage.LimitedGetter)
	if !ok {
		return nil, errors.New("catalog requires bounded reads")
	}
	versioned, ok := store.(storage.CatalogVersionedGetter)
	if !ok {
		return nil, errors.New("catalog requires versioned reads")
	}
	conditional, ok := store.(storage.ConditionalPutter)
	if !ok {
		return nil, errors.New("catalog requires conditional writes")
	}
	clock, ok := store.(storage.CatalogClock)
	if !ok {
		return nil, errors.New("catalog requires qualified provider clock")
	}
	return &Writer{store: store, bounded: bounded, versioned: versioned, conditional: conditional, clock: clock}, nil
}

// NewMutationID creates a fresh publication and revision identity.
func NewMutationID() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (w *Writer) putJSON(ctx context.Context, kind ImmutableKind, value any, limit int) (ObjectRef, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return ObjectRef{}, err
	}
	if len(b) > limit {
		return ObjectRef{}, storage.ErrObjectTooLarge
	}
	return w.PutImmutable(ctx, kind, b)
}

// PutImmutable creates exact content-addressed objects without replacement.
func (w *Writer) PutImmutable(ctx context.Context, kind ImmutableKind, b []byte) (ObjectRef, error) {
	sum := storage.SHA256Hex(b)
	key := "catalog-v4/" + string(kind) + "/" + sum + ".json"
	if kind != KindNodes && kind != KindMetadata && kind != KindHeads {
		return ObjectRef{}, errors.New("invalid immutable object kind")
	}
	_, err := w.conditional.PutConditional(ctx, key, b, storage.PutCondition{CreateOnly: true})
	if err != nil {
		raw, readErr := w.readRef(ctx, ObjectRef{key, sum}, int64(len(b)))
		if readErr != nil {
			return ObjectRef{}, errors.Join(err, readErr)
		}
		if string(raw) != string(b) {
			return ObjectRef{}, storage.ErrChecksumMismatch
		}
	}
	return ObjectRef{key, sum}, nil
}

// Head reads the current commit point and its exact provider validator.
func (w *Writer) Head(ctx context.Context) (CatalogHead, string, error) {
	h, v, err := w.readHead(ctx)
	return h, v.ETag, err
}

func (w *Writer) readHead(ctx context.Context) (CatalogHead, storage.CatalogObjectVersion, error) {
	raw, version, err := w.versioned.GetCatalogVersion(ctx, HeadKey, 16<<10)
	etag := version.ETag
	if errors.Is(err, storage.ErrNotFound) {
		return CatalogHead{Schema: 4}, storage.CatalogObjectVersion{}, nil
	}
	if err != nil {
		return CatalogHead{}, storage.CatalogObjectVersion{}, err
	}
	if len(raw) > 16<<10 {
		return CatalogHead{}, storage.CatalogObjectVersion{}, storage.ErrObjectTooLarge
	}
	var h CatalogHead
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&h); err != nil {
		return h, storage.CatalogObjectVersion{}, err
	}
	if h.Schema != 4 || h.Generation == 0 || h.Epoch == "" || h.PublicationEpoch == "" || etag == "" {
		return h, storage.CatalogObjectVersion{}, errors.New("invalid catalog head")
	}
	if err = h.observeVersion(version); err != nil {
		return h, storage.CatalogObjectVersion{}, err
	}
	return h, version, nil
}

// Find resolves a canonical session key through the current identity root.
func (w *Writer) Find(ctx context.Context, key string) (*CatalogEntry, string, error) {
	h, _, err := w.Head(ctx)
	if err != nil {
		return nil, "", err
	}
	r, err := w.find(ctx, h.Identity, key)
	return r.Entry, r.Revision, err
}

func (w *Writer) find(ctx context.Context, root ObjectRef, key string) (record, error) {
	b, err := w.lookup(ctx, root, key)
	var r record
	if err == nil && b != nil {
		err = json.Unmarshal(b, &r)
	}
	return r, err
}

func orderKeys(key string, e *CatalogEntry) []string {
	activity := listingindex.ActivityTime(e.Summary)
	return []string{e.Summary.CapturedAt.UTC().Format("2006-01-02T15:04:05.000000000Z") + "/" + key, activity.UTC().Format("2006-01-02T15:04:05.000000000Z") + "/" + key, e.Summary.ProjectID + "/" + key}
}

// Commit rebases only across other sessions. Every acknowledged mutation has
// a durable receipt; an ambiguous response never licenses a blind overwrite.
func (w *Writer) Commit(ctx context.Context, m CatalogMutation) (string, error) {
	m, digest, err := freezeMutation(m)
	if err != nil {
		return "", err
	}
	revision, err := NewMutationID()
	if err != nil {
		return "", err
	}
	for range 64 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		h, etag, err := w.Head(ctx)
		if err != nil {
			return "", err
		}
		if h.GCLease != "" {
			return "", errors.New("catalog garbage collection is fenced")
		}
		committed, err := w.receipt(ctx, h, m.ID, digest)
		if err != nil || committed != "" {
			return committed, err
		}
		next, err := w.prepareCommit(ctx, h, m, revision, digest, etag != "")
		if err != nil {
			return "", err
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return "", err
		}
		_, putErr := w.conditional.PutConditional(ctx, HeadKey, raw, storage.PutCondition{MatchETag: etag, CreateOnly: etag == ""})
		if putErr == nil {
			return revision, nil
		}
		// A definite CAS conflict may rebase. Every other failed response requires
		// fresh receipt authority, or remains unknown without blindly retrying.
		fresh, _, readErr := w.Head(ctx)
		if readErr != nil {
			return "", errors.Join(ErrCommitUnknown, putErr, readErr)
		}
		committed, readErr = w.receipt(ctx, fresh, m.ID, digest)
		if readErr != nil {
			return "", errors.Join(ErrCommitUnknown, putErr, readErr)
		}
		if committed != "" {
			return committed, nil
		}
		if !errors.Is(putErr, storage.ErrPreconditionFailed) {
			return "", errors.Join(ErrCommitUnknown, putErr)
		}
	}
	return "", fmt.Errorf("catalog contention exceeded retry bound: %w", storage.ErrPreconditionFailed)
}

func freezeMutation(m CatalogMutation) (CatalogMutation, string, error) {
	if m.ID == "" || len(m.ID) > 128 || m.SessionKey == "" || len(m.SessionKey) > 1024 {
		return m, "", errors.New("invalid catalog mutation")
	}
	if m.Next != nil && (m.Next.Metadata.Key == "" || m.Next.Metadata.SHA256 == "") {
		return m, "", errors.New("metadata reference required")
	}
	b, err := json.Marshal(m) // #nosec G117 -- SessionKey is an archive object identity, never credential material.
	if err != nil {
		return m, "", err
	}
	// Freeze caller-owned slices, maps and pointers for all retries.
	var frozen CatalogMutation
	if err = json.Unmarshal(b, &frozen); err != nil {
		return m, "", err
	}
	return frozen, storage.SHA256Hex(b), nil
}

func (w *Writer) receipt(ctx context.Context, h CatalogHead, id, digest string) (string, error) {
	b, err := w.lookup(ctx, h.Receipts, id)
	if err != nil || b == nil {
		return "", err
	}
	var r receipt
	if err = json.Unmarshal(b, &r); err != nil {
		return "", err
	}
	if r.Digest != digest || r.Revision == "" || r.SessionKey == "" {
		return "", ErrMutationReuse
	}
	return r.Revision, nil
}

func (w *Writer) prepareCommit(ctx context.Context, h CatalogHead, m CatalogMutation, revision, digest string, existing bool) (CatalogHead, error) {
	base := h
	if m.Next != nil {
		if err := w.verifyEntry(ctx, m.SessionKey, m.Next); err != nil {
			return h, err
		}
	}
	old, err := w.find(ctx, h.Identity, m.SessionKey)
	if err != nil {
		return h, err
	}
	if old.Revision != m.ExpectedRevision {
		return h, ErrConflict
	}
	next := m.Next
	if next != nil {
		next.Revision = revision
	}
	if err = w.updateOrders(ctx, &h, m.SessionKey, old.Entry, next); err != nil {
		return h, err
	}
	h.Identity, err = w.update(ctx, h.Identity, m.SessionKey, record{revision, next})
	if err != nil {
		return h, err
	}
	metadataHash := ""
	if next != nil {
		metadataHash = next.Metadata.SHA256
	}
	h.Receipts, err = w.update(ctx, h.Receipts, m.ID, receipt{Digest: digest, Revision: revision, SessionKey: m.SessionKey, MetadataSHA256: metadataHash})
	if err != nil {
		return h, err
	}
	if existing {
		h.PredecessorWitness = base.PublicationWitness
		h.Previous, err = w.putJSON(ctx, KindHeads, base, 16<<10)
		if err != nil {
			return h, err
		}
	}
	h.Generation++
	h.Epoch = revision
	h.CommittedAt = time.Time{}
	h.PublicationEpoch = revision
	h.PublicationWitness = HeadWitness{}
	return h, nil
}

func (w *Writer) updateOrders(ctx context.Context, h *CatalogHead, key string, old, next *CatalogEntry) error {
	roots := []*ObjectRef{&h.Capture, &h.Activity, &h.Project}
	for _, entry := range []*CatalogEntry{old, next} {
		if entry == nil {
			continue
		}
		var value any
		if entry == next {
			value = entry
		}
		for i, orderKey := range orderKeys(key, entry) {
			ref, err := w.update(ctx, *roots[i], orderKey, value)
			if err != nil {
				return err
			}
			*roots[i] = ref
		}
	}
	return nil
}

func (w *Writer) verifyEntry(ctx context.Context, key string, entry *CatalogEntry) error {
	raw, err := w.readRef(ctx, entry.Metadata, 32<<20)
	if err != nil {
		return err
	}
	var metadata archive.Metadata
	if err = json.Unmarshal(raw, &metadata); err != nil {
		return err
	}
	canonical, err := archive.MetadataObjectKey(metadata.Harness.Name, metadata.SessionID)
	if err != nil || canonical != key {
		return errors.New("catalog metadata identity mismatch")
	}
	expected, _ := json.Marshal(entry.Summary)
	actual, _ := json.Marshal(metadata)
	if string(expected) != string(actual) {
		return errors.New("catalog summary does not match immutable metadata")
	}
	refs, err := metadata.SourceReferences()
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if statter, ok := w.store.(storage.ObjectStatter); ok {
			info, e := statter.Stat(ctx, ref.Key)
			if e != nil {
				return e
			}
			if info.SHA256 != "" {
				if info.SHA256 != ref.SHA256 || info.Size != int64(ref.CompressedBytes) {
					return storage.ErrChecksumMismatch
				}
				continue
			}
		}
		if _, err = w.readRef(ctx, ObjectRef{ref.Key, ref.SHA256}, int64(ref.CompressedBytes)); err != nil {
			return err
		}
	}
	return nil
}

func (h *CatalogHead) rootsSHA256() string {
	b, _ := json.Marshal(struct {
		Identity    ObjectRef   `json:"Identity"`
		Capture     ObjectRef   `json:"Capture"`
		Activity    ObjectRef   `json:"Activity"`
		Project     ObjectRef   `json:"Project"`
		Receipts    ObjectRef   `json:"Receipts"`
		Previous    ObjectRef   `json:"Previous"`
		Epoch       string      `json:"Epoch"`
		Predecessor HeadWitness `json:"Predecessor"`
	}{h.Identity, h.Capture, h.Activity, h.Project, h.Receipts, h.Previous, h.PublicationEpoch, h.PredecessorWitness})
	return storage.SHA256Hex(b)
}

func (h *CatalogHead) observeVersion(v storage.CatalogObjectVersion) error {
	if v.LastModified.IsZero() || v.Precision <= 0 || v.Precision > time.Minute {
		return errors.New("catalog version timestamp precision is unqualified")
	}
	witness := h.PublicationWitness
	if witness.ETag == "" {
		if h.Epoch != h.PublicationEpoch || h.GCLease != "" {
			return errors.New("catalog lease lost publication witness")
		}
		witness = HeadWitness{ETag: v.ETag, LastModified: v.LastModified, Precision: v.Precision, Epoch: h.PublicationEpoch, RootsSHA256: h.rootsSHA256()}
	}
	if witness.ETag == "" || witness.LastModified.IsZero() || witness.Precision <= 0 || witness.Precision > time.Minute || witness.Precision != v.Precision || witness.Epoch != h.PublicationEpoch || witness.RootsSHA256 != h.rootsSHA256() || v.LastModified.Add(v.Precision).Before(witness.LastModified) {
		return errors.New("catalog publication witness differs from observed version")
	}
	if h.Previous.Key != "" {
		prior := h.PredecessorWitness
		if prior.ETag == "" || prior.LastModified.IsZero() || prior.Precision <= 0 || prior.Precision > time.Minute || prior.LastModified.After(witness.LastModified.Add(witness.Precision)) {
			return errors.New("catalog predecessor publication clock regressed or is unqualified")
		}
	}
	h.PublicationWitness = witness
	h.CommittedAt = witness.LastModified
	return nil
}
