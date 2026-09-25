package retention

import (
	"context"
	"fmt"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// DeleteWholeSession removes a session from the bucket: its metadata, then
// every object under sessions/<harness>/<archive id>/. Discovery goes first:
// if source deletion is interrupted, unreferenced objects remain for the next
// attempt, but no live metadata points at missing data. Whole-session
// retention and backfill undo both delete through it.
func DeleteWholeSession(ctx context.Context, store storage.ObjectStore, harness, archiveSessionID string) error {
	metadataKey, err := archive.MetadataObjectKey(harness, archiveSessionID)
	if err != nil {
		return err
	}
	if err := store.Delete(ctx, metadataKey); err != nil {
		return fmt.Errorf("delete metadata: %w", err)
	}
	prefix := fmt.Sprintf("sessions/%s/%s/", harness, archiveSessionID)
	objects, err := store.List(ctx, prefix)
	if err != nil {
		return fmt.Errorf("list %q: %w", prefix, err)
	}
	for _, obj := range objects {
		if err := store.Delete(ctx, obj.Key); err != nil {
			return fmt.Errorf("delete %q: %w", obj.Key, err)
		}
	}
	return nil
}
