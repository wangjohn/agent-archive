package retention

import (
	"bytes"
	"errors"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestHistoryDeletionLeavesEveryArtifactUnchanged(t *testing.T) {
	t.Parallel()
	store := storagetest.NewMemoryStore()
	ctx := t.Context()
	objects := map[string][]byte{"sessions/codex/protected/metadata.json": []byte(`{"schema_version":2,"history":{"current_revision":"11111111-1111-4111-8111-111111111111"}}`), "sessions/codex/protected/source.active.jsonl.gz": []byte("active"), "sessions/codex/protected/source.preserved.jsonl.gz": []byte("preserved")}
	for key, raw := range objects {
		if e := store.Put(ctx, key, raw); e != nil {
			t.Fatal(e)
		}
	}
	if e := DeleteWholeSession(ctx, store, "codex", "protected"); !errors.Is(e, archive.ErrHistoryMutationPending) {
		t.Fatalf("delete history: %v", e)
	}
	for key, raw := range objects {
		got, e := store.Get(ctx, key)
		if e != nil || !bytes.Equal(raw, got) {
			t.Fatalf("changed %s: %v", key, e)
		}
	}
}
