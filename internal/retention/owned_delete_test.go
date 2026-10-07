package retention

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
	"os"
	"strings"
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

type hookBeforeIntentStore struct {
	storage.ObjectStore
	once    sync.Once
	hook    func()
	deletes int
}

func (s *hookBeforeIntentStore) Get(ctx context.Context, key string) ([]byte, error) {
	r, err := s.ObjectStore.Get(ctx, key)
	if strings.HasSuffix(key, "/metadata.json") {
		s.once.Do(s.hook)
	}
	return r, err
}
func (s *hookBeforeIntentStore) Delete(ctx context.Context, key string) error {
	s.deletes++
	return s.ObjectStore.Delete(ctx, key)
}
func TestRetentionHookBeforeFirstIntentNeverBecomesDecisionAuthority(t *testing.T) {
	local := newTestStore(t)
	remote := storagetest.NewMemoryStore()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reg := registration("s1", writeTranscript(t, t.TempDir(), "s1.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	collect(t, local, remote, at)
	expiry := at.Add(91 * 24 * time.Hour)
	var hookErr error
	wrapped := &hookBeforeIntentStore{ObjectStore: remote, hook: func() { hookErr = local.SaveRequest(reg.ArchiveSessionID, "stop", expiry, finalResponse(t, expiry)) }}
	result := sweep(t, local, wrapped, expiry, Options{})
	if hookErr != nil || len(result.Errors) != 0 || len(result.DeletedSessions) != 0 || wrapped.deletes != 0 {
		t.Fatal("pre-intent hook was covered by deletion", result, hookErr, wrapped.deletes)
	}
	if _, found, err := local.LoadSessionDeletion(reg); err != nil || found {
		t.Fatal("pre-intent work committed deletion", found, err)
	}
	if req, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || !found || len(req.HookEvidence) != 1 {
		t.Fatal("new request lost", req, err)
	}
}
func TestRetentionInterruptedIntentResumesBeforeNewerRequestDeferral(t *testing.T) {
	local := newTestStore(t)
	remote := storagetest.NewMemoryStore()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reg := registration("s1", writeTranscript(t, t.TempDir(), "s1.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	collect(t, local, remote, at)
	expiry := at.Add(91 * 24 * time.Hour)
	if err := DeleteOwnedSession(t.Context(), local, &uncertainMetadataDelete{ObjectStore: remote}, reg, state.RemovalReasonRetention, expiry); err == nil {
		t.Fatal("uncertain delete accepted")
	}
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", expiry, finalResponse(t, expiry)); err != nil {
		t.Fatal(err)
	}
	restarted, err := state.Open(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	result := sweep(t, restarted, remote, expiry, Options{})
	if len(result.Errors) != 0 || len(result.DeletedSessions) != 0 {
		t.Fatal("new work dropped or intent stalled", result)
	}
	j, found, err := restarted.LoadSessionDeletion(reg)
	if err != nil || !found || j.Phase != "absent" {
		t.Fatal("request prevented exact intent resumption", j, err)
	}
	if req, found, err := restarted.LoadRequest(reg.ArchiveSessionID); err != nil || !found || len(req.HookEvidence) != 1 {
		t.Fatal("new hook lost", req, err)
	}
	completed := collect(t, restarted, remote, expiry.Add(time.Minute))
	if len(completed.Errors) != 0 || len(completed.Published) != 1 {
		t.Fatal("resumed exact retention cannot restore", completed)
	}
}
func TestRetentionExcludedOldDecisionRequestStillExpires(t *testing.T) {
	local := newTestStore(t)
	remote := storagetest.NewMemoryStore()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reg := registration("s1", writeTranscript(t, t.TempDir(), "s1.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	collect(t, local, remote, at)
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	result := sweep(t, local, remote, at.Add(91*24*time.Hour), Options{Publishable: func(archive.SessionRegistration) bool { return false }})
	if len(result.Errors) != 0 || len(result.DeletedSessions) != 1 {
		t.Fatal("known excluded work stalled cleanup", result)
	}
	j, found, err := local.LoadSessionDeletion(reg)
	if err != nil || !found || !j.CoveredRequest || !j.LocalRemoved {
		t.Fatal("covered decision not durable", j, err)
	}
}

type cancelSelectingRead struct {
	storage.ObjectStore
	cancel                 context.CancelFunc
	metadataReads, deletes int
}

func (s *cancelSelectingRead) Get(ctx context.Context, key string) ([]byte, error) {
	raw, err := s.ObjectStore.Get(ctx, key)
	if strings.HasSuffix(key, "/metadata.json") {
		s.metadataReads++
		if s.metadataReads == 2 {
			s.cancel()
		}
	}
	return raw, err
}
func (s *cancelSelectingRead) Delete(ctx context.Context, key string) error {
	s.deletes++
	return s.ObjectStore.Delete(ctx, key)
}
func TestOwnedDeletionCancellationAfterSelectingRecheckRetainsAuthority(t *testing.T) {
	local := newTestStore(t)
	remote := storagetest.NewMemoryStore()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reg := registration("s1", writeTranscript(t, t.TempDir(), "s1.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	collect(t, local, remote, at)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	wrapped := &cancelSelectingRead{ObjectStore: remote, cancel: cancel}
	if err := DeleteOwnedSession(ctx, local, wrapped, reg, state.RemovalReasonUndo, at); !errors.Is(err, context.Canceled) || wrapped.deletes != 0 {
		t.Fatal("cancelled selecting proof allowed deletion", err, wrapped.deletes)
	}
	if j, found, err := local.LoadSessionDeletion(reg); err != nil || !found || j.Phase != "deleting" {
		t.Fatal("cancelled intent not recoverable", j, err)
	}
	if err := DeleteOwnedSession(t.Context(), local, remote, reg, state.RemovalReasonUndo, at); err != nil {
		t.Fatal("cancelled exact intent did not resume", err)
	}
}
