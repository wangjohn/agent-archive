package retention

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// DeleteOwnedSession journals a metadata-first deletion for the unsynced local
// owner. A hook arriving during retention keeps sources for exact restoration.

func DeleteOwnedSession(ctx context.Context, local *state.Store, store storage.ObjectStore, reg archive.SessionRegistration, reason state.RemovalReason, now time.Time) error {
	return deleteOwnedAtDecision(ctx, local, store, reg, reason, now, nil)
}
func deleteOwnedAtDecision(ctx context.Context, local *state.Store, store storage.ObjectStore, reg archive.SessionRegistration, reason state.RemovalReason, now time.Time, token *string) error {
	key, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if err != nil {
		return err
	}
	raw, err := storage.ReadPublicationMetadata(ctx, store, key)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	j, found, err := local.LoadSessionDeletion(reg)
	if err != nil {
		return err
	}
	if !found || j.Phase == "restored" || reason == state.RemovalReasonUndo && j.Reason == state.RemovalReasonRetention {
		// Preserve the shipped history entrypoint fence; the underlying selection
		// validation/journal also understands every preserved ref for enablement.
		if err = checkDeletionMetadata(raw); err != nil {
			return err
		}
		if reason == state.RemovalReasonRetention && token != nil {
			j, err = local.PrepareRetentionDeletion(reg, raw, now, *token)
		} else {
			j, err = local.PrepareSessionDeletion(reg, reason, raw, now)
		}
		if err != nil {
			return err
		}
	}
	if j.Reason != reason || j.Phase == "restored" || j.Phase == "restoring" {
		return state.ErrAdmissionStageRecovery
	}
	if len(raw) > 0 && storage.SHA256Hex(raw) != j.MetadataSHA256 {
		return storage.ErrPublicationConflict
	}
	if err = deleteOwnedMetadata(ctx, local, store, reg, &j, key, raw); err != nil {
		return err
	}
	return deleteOwnedNamespace(ctx, local, store, reg, j, key)
}
func deleteOwnedMetadata(ctx context.Context, local *state.Store, store storage.ObjectStore, reg archive.SessionRegistration, j *state.SessionDeletion, key string, raw []byte) error {
	var err error
	if j.Phase == "prepared" {
		if len(raw) == 0 && j.MetadataSHA256 != "" {
			return storage.ErrPublicationConflict
		}
		if err = verifyDeletionReferences(ctx, store, j.References); err != nil {
			return err
		}
		if err = local.AdvanceSessionDeletion(reg, "deleting"); err != nil {
			return err
		}
		j.Phase = "deleting"
	}
	if j.Phase == "deleting" {
		fresh, e := storage.ReadPublicationMetadata(ctx, store, key)
		if e != nil && !errors.Is(e, storage.ErrNotFound) {
			return e
		}
		if e == nil && storage.SHA256Hex(fresh) != j.MetadataSHA256 {
			return storage.ErrPublicationConflict
		}
		if err = store.Delete(ctx, key); err != nil {
			return err
		}
		if _, err = storage.ReadPublicationMetadata(ctx, store, key); !errors.Is(err, storage.ErrNotFound) {
			if err == nil {
				return storage.ErrPublicationConflict
			}
			return err
		}
		if err = local.AdvanceSessionDeletion(reg, "absent"); err != nil {
			return err
		}
		j.Phase = "absent"
	}
	return nil
}
func deleteOwnedNamespace(ctx context.Context, local *state.Store, store storage.ObjectStore, reg archive.SessionRegistration, j state.SessionDeletion, key string) error {
	var err error
	if j.Phase == "cleaned" {
		return nil
	}
	if err = listingindex.DeleteSession(ctx, store, reg.Harness.Name, reg.ArchiveSessionID); err != nil {
		return err
	}
	prefix := fmt.Sprintf("sessions/%s/%s/", reg.Harness.Name, reg.ArchiveSessionID)
	objects, err := store.List(ctx, prefix)
	if err != nil {
		return err
	}
	for _, object := range objects {
		if err = ctx.Err(); err != nil {
			return err
		}
		if _, err = storage.ReadPublicationMetadata(ctx, store, key); !errors.Is(err, storage.ErrNotFound) {
			if err == nil {
				return storage.ErrPublicationConflict
			}
			return err
		}
		if j.Reason == state.RemovalReasonRetention {
			req, have, e := local.LoadRequest(reg.ArchiveSessionID)
			if e != nil {
				return e
			}
			if have && req.Token != j.RequestToken {
				return nil
			}
		}
		if err = store.Delete(ctx, object.Key); err != nil {
			return err
		}
	}
	return local.AdvanceSessionDeletion(reg, "cleaned")
}

func verifyDeletionReferences(ctx context.Context, store storage.ObjectStore, refs []archive.SourceReference) error {
	if len(refs) == 0 {
		return nil
	}
	sources := make([]storage.SourcePublication, len(refs))
	for i, ref := range refs {
		sources[i] = storage.SourcePublication{Key: ref.Key, SHA256: ref.SHA256, Size: ref.CompressedBytes}
	}
	return storage.VerifySourceSet(ctx, store, sources, storage.RetryPolicy{})
}
