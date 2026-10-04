package collector

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type failingListingStore struct {
	*storagetest.MemoryStore
	fail  bool
	fault error
}

func (s *failingListingStore) Put(ctx context.Context, key string, data []byte) error {
	if s.fail && strings.HasPrefix(key, listingindex.V2Prefix) {
		return s.fault
	}
	return s.MemoryStore.Put(ctx, key, data)
}

// An auxiliary failure acknowledges capture, persists its retry intent, and a
// later pass repairs it without republishing canonical metadata or sources.
func TestListingRepairFailureDoesNotLoseCapture(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	claudeSession(t, local, claudePromptLine+"\n")
	fault := errors.New("auxiliary upload unavailable")
	remote := &failingListingStore{MemoryStore: storagetest.NewMemoryStore(), fail: true, fault: fault}
	at := time.Date(2026, 9, 22, 13, 0, 0, 0, time.UTC)
	result := runAt(t, local, remote, at)
	if len(result.Published) != 1 || !errors.Is(result.Errors["session-1"], fault) {
		t.Fatalf("capture not acknowledged with maintenance warning: %+v", result)
	}
	repairs, err := local.ListingRepairs(32)
	if err != nil || len(repairs) != 1 {
		t.Fatalf("durable work lost: %v %v", repairs, err)
	}
	before, err := remote.Stat(context.Background(), "sessions/claude/session-1/metadata.json")
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	local = reopened
	remote.fail = false
	result = runAt(t, local, remote, at.Add(time.Minute))
	if len(result.Published) != 0 || len(result.Errors) != 0 {
		t.Fatalf("repair republished or failed: %+v", result)
	}
	after, err := remote.Stat(context.Background(), "sessions/claude/session-1/metadata.json")
	if err != nil || before.ETag != after.ETag {
		t.Fatal("repair rewrote canonical metadata")
	}
	repairs, err = local.ListingRepairs(32)
	if err != nil || len(repairs) != 0 {
		t.Fatalf("repair not acknowledged: %v %v", repairs, err)
	}
	scanned := false
	listed, err := reader.ListRecent(context.Background(), remote, "sessions", reader.Filter{}, 1, reader.ListOptions{CompatibilityScan: func(string) { scanned = true }})
	if err != nil || scanned || len(listed.Sessions) != 1 {
		t.Fatalf("repaired coverage invalid: scanned=%v err=%v", scanned, err)
	}
}
