package retention

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type uncertainMetadataDelete struct {
	storage.ObjectStore
	once sync.Once
}

func (s *uncertainMetadataDelete) Delete(ctx context.Context, key string) error {
	err := s.ObjectStore.Delete(ctx, key)
	injected := false
	s.once.Do(func() { injected = true })
	if injected && err == nil {
		return errors.New("synthetic uncertain delete result")
	}
	return err
}

func TestOwnedDeletionResumesUncertainMetadataDeleteWithoutNative(t *testing.T) {
	local := newTestStore(t)
	memory := storagetest.NewMemoryStore()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reg := registration("s1", writeTranscript(t, t.TempDir(), "s1.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	collect(t, local, memory, at)
	faulty := &uncertainMetadataDelete{ObjectStore: memory}
	if err := DeleteOwnedSession(context.Background(), local, faulty, reg, state.RemovalReasonUndo, at.Add(time.Hour)); err == nil {
		t.Fatal("uncertain delete reported success")
	}
	journal, found, err := local.LoadSessionDeletion(reg)
	if err != nil || !found || journal.Phase != "deleting" {
		t.Fatal("uncertain result lost exact authority", journal, err)
	}
	objects, err := memory.List(context.Background(), "sessions/codex/s1/")
	if err != nil || len(objects) == 0 {
		t.Fatal("source removed before durable absence", objects, err)
	}
	if err = os.Remove(reg.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	restarted, err := state.Open(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	if err = DeleteOwnedSession(context.Background(), restarted, memory, reg, state.RemovalReasonUndo, at.Add(2*time.Hour)); err != nil {
		t.Fatal("journaled deletion did not resume", err)
	}
	objects, err = memory.List(context.Background(), "sessions/codex/s1/")
	if err != nil || len(objects) != 0 {
		t.Fatal("namespace not completely removed", objects, err)
	}
	journal, found, err = restarted.LoadSessionDeletion(reg)
	if err != nil || !found || journal.Phase != "cleaned" {
		t.Fatal("cleanup authority not committed", journal, err)
	}
}
