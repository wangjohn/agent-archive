package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/wangjohn/agent-archive/internal/storage"
)

// CoordinatorKey is the destination-wide durable admission authority. Catalog
// garbage collection must preserve it independently from tree reachability.
const CoordinatorKey = "catalog-v4/coordinator.json"

// ErrAdmissionClosed keeps all unacknowledged owners and the seal durable.
var ErrAdmissionClosed = errors.New("catalog admission sealed or owners not drained")

type admission struct {
	Digest string      `json:"digest"`
	Refs   []ObjectRef `json:"refs"`
}

type admissions struct {
	Protocol   uint64               `json:"protocol"`
	Mode       string               `json:"mode"`
	Proof      string               `json:"proof"`
	Generation uint64               `json:"generation"`
	Seal       string               `json:"seal"`
	Hold       string               `json:"hold"`
	Owners     map[string]admission `json:"owners"`
}

// Coordinator uses the qualified provider's single CAS admission authority.
// Every mutation registers before writing and acknowledges only after its
// durable lifecycle settles. No timeout releases an unfinished owner or seal.
type Coordinator struct{ writer *Writer }

// Coordinator returns the writer's destination-wide admission authority.
func (w *Writer) Coordinator() *Coordinator { return &Coordinator{writer: w} }

func (c *Coordinator) read(ctx context.Context) (admissions, string, error) {
	raw, v, err := c.writer.versioned.GetCatalogVersion(ctx, CoordinatorKey, 4<<20)
	if errors.Is(err, storage.ErrNotFound) {
		return admissions{Protocol: 9, Mode: "candidate", Owners: map[string]admission{}}, "", nil
	}
	if err != nil {
		return admissions{}, "", err
	}
	var state admissions
	if json.Unmarshal(raw, &state) != nil || state.Protocol != 9 || state.Generation == 0 || state.Owners == nil || v.ETag == "" {
		return state, "", errors.New("invalid catalog coordinator")
	}
	return state, v.ETag, nil
}

func (c *Coordinator) change(ctx context.Context, fn func(*admissions) error) error {
	for range 64 {
		state, etag, err := c.read(ctx)
		if err != nil {
			return err
		}
		if err = fn(&state); err != nil {
			return err
		}
		state.Generation++
		raw, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if len(raw) > 4<<20 {
			return storage.ErrObjectTooLarge
		}
		_, err = c.writer.conditional.PutConditional(ctx, CoordinatorKey, raw, storage.PutCondition{MatchETag: etag, CreateOnly: etag == ""})
		if err == nil {
			return nil
		}
		if !errors.Is(err, storage.ErrPreconditionFailed) {
			return errors.Join(ErrCommitUnknown, err)
		}
	}
	return storage.ErrPreconditionFailed
}

// Admit registers exact frozen lifecycle input before any mutation. Identical
// owners are refused to prevent concurrent acknowledgement of another caller.
// A new owner cannot cross a seal, including after process restart.
func (c *Coordinator) Admit(ctx context.Context, owner, digest string, refs []ObjectRef) error {
	if owner == "" || len(owner) > 512 || digest == "" || len(refs) > 256 {
		return errors.New("invalid catalog admission")
	}
	for _, ref := range refs {
		if ref.Key == "" || ref.SHA256 == "" {
			return errors.New("incomplete catalog admission refs")
		}
	}
	return c.change(ctx, func(state *admissions) error {
		if _, ok := state.Owners[owner]; ok {
			return ErrAdmissionClosed
		}
		if state.Seal != "" {
			return ErrAdmissionClosed
		}
		if len(state.Owners) >= 256 {
			return errors.New("catalog admission owner capacity exceeded")
		}
		state.Owners[owner] = admission{Digest: digest, Refs: refs}
		return nil
	})
}

// Complete acknowledges a settled frozen owner. Callers must hold its durable
// pending authority until all history/preservation and publication work ends.
// A crash before acknowledgement intentionally blocks collection and cutover.
func (c *Coordinator) Complete(ctx context.Context, owner, digest string) error {
	return c.change(ctx, func(state *admissions) error {
		if state.Hold != "" {
			return ErrAdmissionClosed
		}
		prior, ok := state.Owners[owner]
		if !ok {
			return nil
		}
		if prior.Digest != digest {
			return ErrMutationReuse
		}
		delete(state.Owners, owner)
		return nil
	})
}

// Seal closes new admissions durably. Existing owners must acknowledge their
// frozen lifecycle before inventory is complete; references alone never prove
// stopped writers. The returned owner is required for explicit crash recovery.
func (c *Coordinator) Seal(ctx context.Context) (string, error) {
	owner, err := NewMutationID()
	if err != nil {
		return "", err
	}
	err = c.change(ctx, func(state *admissions) error {
		if state.Seal != "" {
			return ErrAdmissionClosed
		}
		state.Seal = owner
		return nil
	})
	return owner, err
}

// HeldBarrier binds collection/migration to the exact durable seal owner.
// Hold verifies all lifecycle owners drained and returns a release that checks
// that same owner. An error leaves admission closed for explicit recovery.
func (c *Coordinator) HeldBarrier(owner string) Barrier { return heldCoordinator{c, owner} }

type heldCoordinator struct {
	coordinator *Coordinator
	owner       string
}

func (b heldCoordinator) Hold(ctx context.Context) ([]ObjectRef, func(), error) {
	hold, err := NewMutationID()
	if err != nil {
		return nil, nil, err
	}
	err = b.coordinator.change(ctx, func(state *admissions) error {
		if b.owner == "" || state.Seal != b.owner || len(state.Owners) != 0 || state.Hold != "" {
			return ErrAdmissionClosed
		}
		state.Hold = hold
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return nil, func() { _ = b.coordinator.releaseHeld(context.WithoutCancel(ctx), b.owner, hold) }, nil
}

// Release is explicit owner-checked recovery. Unfinished owners always refuse.
func (c *Coordinator) Release(ctx context.Context, owner string) error {
	if owner == "" {
		return ErrAdmissionClosed
	}
	return c.change(ctx, func(state *admissions) error {
		if state.Seal != owner || len(state.Owners) != 0 || state.Hold != "" {
			return ErrAdmissionClosed
		}
		state.Seal = ""
		return nil
	})
}

// BeginPublication holds global pending/history admission through durable local
// acknowledgement. Frozen metadata describes all active and preserved sources.
func (s *Store) BeginPublication(ctx context.Context, id string, metadata []byte) error {
	refs, digest, err := publicationAdmission(metadata)
	if err != nil {
		return err
	}
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if s.running[id] {
		return ErrAdmissionClosed
	}
	if prior, ok := s.pending[id]; ok {
		if prior != digest {
			return ErrMutationReuse
		}
		s.running[id] = true
		return nil
	}
	if err = s.Writer.Coordinator().Admit(ctx, "pending/"+id, digest, refs); err != nil {
		return err
	}
	if s.pending == nil {
		s.pending = map[string]string{}
	}
	s.pending[id] = digest
	if s.running == nil {
		s.running = map[string]bool{}
	}
	s.running[id] = true
	return nil
}

// CompletePublication settles the exact acknowledged pending lifecycle.
func (s *Store) CompletePublication(ctx context.Context, id string, metadata []byte) error {
	_, digest, err := publicationAdmission(metadata)
	if err != nil {
		return err
	}
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if prior, ok := s.pending[id]; !ok || prior != digest || !s.running[id] {
		return ErrAdmissionClosed
	}
	if err = s.Writer.Coordinator().Complete(ctx, "pending/"+id, digest); err != nil {
		return err
	}
	delete(s.pending, id)
	return nil
}

func publicationAdmission(raw []byte) ([]ObjectRef, string, error) {
	var entry CatalogEntry
	if err := json.Unmarshal(raw, &entry.Summary); err != nil {
		return nil, "", err
	}
	sources, err := entry.Summary.SourceReferences()
	if err != nil {
		return nil, "", err
	}
	refs := make([]ObjectRef, 0, len(sources))
	for _, source := range sources {
		refs = append(refs, ObjectRef{source.Key, source.SHA256})
	}
	return refs, storage.SHA256Hex(raw), nil
}

func coordinatorObject(key string) bool {
	return key == CoordinatorKey || strings.HasPrefix(key, "catalog-v4/migrations/")
}

// Activate makes the fully verified candidate visible to general readers.
// Only the exact held migration owner can transition a drained destination.
func (c *Coordinator) Activate(ctx context.Context, owner, proof string) error {
	if proof == "" {
		return errors.New("cutover proof required")
	}
	return c.change(ctx, func(state *admissions) error {
		if state.Seal != owner || len(state.Owners) != 0 || state.Hold != "" {
			return ErrAdmissionClosed
		}
		state.Mode = "active"
		state.Proof = proof
		return nil
	})
}

// Active refuses candidate, interrupted and rollback destinations as complete
// archive discovery authority. Reader activation is independent from presence
// of a head and can never be inferred from a nonempty prefix.
func (c *Coordinator) Active(ctx context.Context) error {
	state, _, err := c.read(ctx)
	if err != nil {
		return err
	}
	if state.Mode != "active" || state.Proof == "" {
		return errors.New("catalog migration is not activated")
	}
	return nil
}

// Deactivate closes reader activation while the migration owner holds writers.
func (c *Coordinator) Deactivate(ctx context.Context, owner string) error {
	return c.change(ctx, func(state *admissions) error {
		if state.Seal != owner || len(state.Owners) != 0 || state.Hold != "" {
			return ErrAdmissionClosed
		}
		state.Mode = "rollback"
		return nil
	})
}

// EndPublicationAttempt releases only the invocation's local execution claim.
// The durable pending admission remains until full lifecycle acknowledgement.
// Another in-process retry may then resume; another process remains refused.
func (s *Store) EndPublicationAttempt(id string) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	delete(s.running, id)
}

func (c *Coordinator) releaseHeld(ctx context.Context, owner, hold string) error {
	return c.change(ctx, func(state *admissions) error {
		if state.Seal != owner || state.Hold != hold || hold == "" || len(state.Owners) != 0 {
			return ErrAdmissionClosed
		}
		state.Hold = ""
		state.Seal = ""
		return nil
	})
}

// CatalogBarrier seals the durable destination admission authority. Hold must
// observe every registered lifecycle drained; crashed owners never age out.
func (s *Store) CatalogBarrier(ctx context.Context) (Barrier, error) {
	owner, err := s.Writer.Coordinator().Seal(ctx)
	if err != nil {
		return nil, err
	}
	return s.Writer.Coordinator().HeldBarrier(owner), nil
}
