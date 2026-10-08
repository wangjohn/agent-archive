package catalog

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/wangjohn/agent-archive/internal/storage"
)

type migrationIntentKind string

const (
	migrationInitialize     migrationIntentKind = "initialize"
	migrationRollbackIntent migrationIntentKind = "rollback"
)

type migrationIntent struct {
	Kind  migrationIntentKind `json:"kind"`
	State CatalogMigration    `json:"state"`
}

type migrationIntentKey struct{}
type migrationIntentAuthority struct {
	writer *Writer
	digest string
}

func intentDigest(intent *migrationIntent) string {
	raw, _ := json.Marshal(intent)
	return storage.SHA256Hex(raw)
}

func (intent *migrationIntent) validate(state admissions) error {
	if intent == nil {
		return nil
	}
	m := intent.State
	if m.validate() != nil || len(state.Owners) != 0 || state.Hold != "" || state.GCLink != nil || (state.Seal != "" && state.Seal != m.Owner) {
		return errCoordinatorDescriptor
	}
	switch intent.Kind {
	case migrationInitialize:
		if m.Phase != MigrationInitializing || m.Copied != 0 || m.Cursor != "" || m.ExpectedHead != "" || m.VerifiedRoot != (ObjectRef{}) || state.Mode != admissionCandidate || state.Proof != "" || len(state.Completed) != 0 {
			return errCoordinatorDescriptor
		}
	case migrationRollbackIntent:
		if m.Phase != MigrationRollbackPreparing || (state.Mode != admissionActive && state.Mode != admissionRollback) || state.Proof != m.Proof {
			return errCoordinatorDescriptor
		}
	default:
		return errCoordinatorDescriptor
	}
	return nil
}

func (m *Migration) intentContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, migrationIntentKey{}, migrationIntentAuthority{m.writer, intentDigest(m.intent)})
}

func (c *Coordinator) changeIntent(ctx context.Context, intent *migrationIntent, fn func(*admissions) error) error {
	ctx = context.WithValue(ctx, migrationIntentKey{}, migrationIntentAuthority{c.writer, intentDigest(intent)})
	var expected []byte
	err := c.change(ctx, func(state *admissions) error {
		if err := fn(state); err != nil {
			return err
		}
		prepared := *state
		prepared.Generation++
		var err error
		expected, err = json.Marshal(prepared)
		return err
	})
	if err == nil {
		return nil
	}
	if len(expected) == 0 {
		return err
	}
	// Compare the complete intended generation and phase after a lost response.
	actual, _, readErr := c.writer.versioned.GetCatalogVersion(ctx, CoordinatorKey, 4<<20)
	if readErr == nil && string(actual) == string(expected) {
		return nil
	}
	return errors.Join(err, readErr)
}

func (m *Migration) beginIntent(ctx context.Context, kind migrationIntentKind) error {
	if _, err := m.writer.preflightClock(ctx); err != nil {
		return err
	}
	intent := &migrationIntent{Kind: kind, State: m.State}
	m.intent = intent
	return m.writer.Coordinator().changeIntent(ctx, intent, func(state *admissions) error {
		if state.Intent != nil {
			if intentDigest(state.Intent) == intentDigest(intent) {
				return nil
			}
			return ErrAdmissionClosed
		}
		if state.Seal != "" || state.Hold != "" || state.GCLink != nil || len(state.Owners) != 0 {
			return ErrAdmissionClosed
		}
		if kind == migrationInitialize {
			if state.Generation != 0 || state.Mode != admissionCandidate || len(state.Completed) != 0 {
				return ErrAdmissionClosed
			}
		} else {
			if state.Mode != admissionActive || state.Proof != m.State.Proof {
				return ErrAdmissionClosed
			}
			if err := m.unchangedHead(ctx); err != nil {
				return err
			}
		}
		state.Intent = intent
		return nil
	})
}

func (m *Migration) sealIntent(ctx context.Context) error {
	if _, err := m.writer.preflightClock(ctx); err != nil {
		return err
	}
	return m.writer.Coordinator().changeIntent(ctx, m.intent, func(state *admissions) error {
		if state.Intent == nil || intentDigest(state.Intent) != intentDigest(m.intent) || (state.Seal != "" && state.Seal != m.State.Owner) {
			return ErrAdmissionClosed
		}
		state.Seal = m.State.Owner
		return nil
	})
}

func (m *Migration) checkIntentCheckpoint(ctx context.Context) error {
	state, _, err := m.writer.Coordinator().read(ctx)
	if err != nil {
		return err
	}
	if state.Intent == nil || intentDigest(state.Intent) != intentDigest(m.intent) || state.Seal != m.State.Owner || state.Intent.validate(state) != nil {
		return ErrAdmissionClosed
	}
	if (m.intent.Kind == migrationInitialize && m.State.Phase != MigrationCopying) || (m.intent.Kind == migrationRollbackIntent && m.State.Phase != MigrationRollbackPreparing && m.State.Phase != MigrationRollback) {
		return ErrAdmissionClosed
	}
	original := m.intent.State
	candidate := m.State
	candidate.Phase = original.Phase
	if intentDigest(&migrationIntent{Kind: m.intent.Kind, State: candidate}) != intentDigest(m.intent) {
		return ErrAdmissionClosed
	}
	return ctx.Err()
}

func (m *Migration) clearIntent(ctx context.Context) error {
	raw, _, err := m.writer.versioned.GetCatalogVersion(ctx, MigrationKey, 64<<10)
	if err != nil {
		return err
	}
	expected, err := json.Marshal(m.State)
	if err != nil || string(raw) != string(expected) {
		return errors.Join(ErrAdmissionClosed, err)
	}
	err = m.writer.Coordinator().changeIntent(ctx, m.intent, func(state *admissions) error {
		if state.Intent == nil || intentDigest(state.Intent) != intentDigest(m.intent) || state.Seal != m.State.Owner {
			return ErrAdmissionClosed
		}
		state.Intent = nil
		return nil
	})
	if err != nil {
		return err
	}
	m.intent = nil
	return nil
}

func (m *Migration) unchangedHead(ctx context.Context) error {
	head, etag, err := m.writer.Head(ctx)
	if err != nil {
		return err
	}
	if head.Identity != m.State.VerifiedRoot || etag != m.State.ExpectedHead {
		return errors.New("catalog changed since activation; rollback would lose publications")
	}
	return nil
}

func (m *Migration) resumeInitialization(ctx context.Context) error {
	if m.intent == nil || m.intent.Kind != migrationInitialize {
		return ErrAdmissionClosed
	}
	objects, err := m.Destination.List(ctx, "")
	if err != nil {
		return err
	}
	for _, object := range objects {
		if object.Key != CoordinatorKey && object.Key != MigrationKey {
			return errors.New("migration initialization contains unrelated objects")
		}
	}
	if err = m.sealIntent(ctx); err != nil {
		return err
	}
	m.State.Phase = MigrationCopying
	if err = m.save(ctx); err != nil {
		return err
	}
	return m.clearIntent(ctx)
}

func (m *Migration) resumeIntent(ctx context.Context, proof CutoverProof) error {
	state, _, err := m.writer.Coordinator().read(ctx)
	if err != nil {
		return err
	}
	if state.Intent == nil {
		return nil
	}
	intent := state.Intent
	bound := intent.State
	if destinationIdentity(bound.Source) != destinationIdentity(m.State.Source) || destinationIdentity(bound.Destination) != destinationIdentity(m.State.Destination) || bound.Proof != proof.ID || (m.State.ID != "" && bound.ID != m.State.ID) {
		return ErrAdmissionClosed
	}
	if intent.Kind == migrationRollbackIntent && (m.checkpointETag == "" || m.State.VerifiedRoot != bound.VerifiedRoot || m.State.ExpectedHead != bound.ExpectedHead || m.State.Copied != bound.Copied || (m.State.Phase != MigrationActive && m.State.Phase != MigrationRollbackPreparing && m.State.Phase != MigrationRollback)) {
		return ErrAdmissionClosed
	}
	if intent.Kind == migrationInitialize && m.checkpointETag != "" && (m.State.Phase != MigrationCopying || m.State.Copied != 0 || m.State.Cursor != "") {
		return ErrAdmissionClosed
	}
	m.intent = intent
	m.State = bound
	return nil
}
