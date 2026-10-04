package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

func packedGenerationFixture(t *testing.T) (*Store, archive.SessionRegistration, time.Time, sessionIndexMarker) {
	t.Helper()
	s, key, marker := packedOwnerFixture(t)
	reg, _, err := s.LoadRegistration("packed-owner")
	if err != nil {
		t.Fatal(err)
	}
	at := reg.RegisteredAt.Add(time.Hour)
	bundle := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: reg.ArchiveSessionID, NativeSessionID: key.NativeID, ProjectID: reg.ProjectID, Capture: archive.SourceCapture{Harness: reg.Harness, CapturedAt: reg.RegisteredAt}}
	p, err := s.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Save(bundle, reg.RegisteredAt, CacheStatusPublished); err != nil {
		t.Fatal(err)
	}
	if err := p.SaveBlocked(bundle, reg.RegisteredAt, BlockedReasonTranscriptRewritten); err != nil {
		t.Fatal(err)
	}
	// Populate the other empty shards so expiry exercises the real packed
	// pruning path without an unrelated missing-shard failure.
	for i := range packedSessionIndexShards {
		shard := packedShardName(i)
		if shard == packedIndexHash(key)[:2] {
			continue
		}
		data, err := encodePackedIndex(packedSessionIndex{Epoch: marker.PackedEpoch, Revision: marker.PackedRevision, Inventory: marker.PackedInventory, Entries: map[string]qualifiedSessionIndexEntry{}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(packedIndexPath(s.home, shard), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return s, reg, at, marker
}

func TestPackedGenerationRecoveryReplayAndCensus(t *testing.T) {
	s, old, at, _ := packedGenerationFixture(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: old.NativeSessionID}
	interrupted := errors.New("interrupted after freeze")
	s.onIndexStep = func(step string) error {
		if step == "generation-frozen" {
			return interrupted
		}
		return nil
	}
	if _, err := s.BeginGenerationRecovery(old.ArchiveSessionID, at, generationBuilder(at)); !errors.Is(err, interrupted) {
		t.Fatalf("freeze seam: %v", err)
	}
	next, found, err := s.GenerationSuccessor(old.ArchiveSessionID)
	if err != nil || !found {
		t.Fatalf("journal: %v %v", found, err)
	}
	if _, found, err := s.ArchiveSessionID(key); found || !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		t.Fatalf("unfinished packed hook routing: %v %v", found, err)
	}
	s.onIndexStep = nil
	if err := s.ResumeGenerationRecoveries(context.Background()); err != nil {
		t.Fatal(err)
	}
	if active, found, err := s.ArchiveSessionID(key); err != nil || !found || active != next {
		t.Fatalf("resumed route: %s %v %v", active, found, err)
	}
	if pending, found, err := s.LoadPending(next); err != nil || !found || !pending.Bundle.Capture.CapturedAt.Equal(at) {
		t.Fatalf("fixed pending snapshot: %v %v", found, err)
	}
	owners := map[agentmeta.SessionKey][]string{key: {old.ArchiveSessionID, next}}
	// Run the actual scheduled large-registration census, including read-ahead,
	// packed phase publication and final certification, with linked duplicates.
	for i := range packedSessionIndexThreshold {
		other := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: fmt.Sprintf("generation-other-%06d", i)}
		reg := migrationRegistration(other, fmt.Sprintf("generation-owner-%06d", i))
		data, err := json.Marshal(reg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.registrationPath(reg.ArchiveSessionID), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	marker := completePackedGenerationCensus(t, s)
	entry, found, err := s.readPackedEntry(packedIndexHash(key), marker)
	if err != nil || !found || entry.Conflict || entry.ArchiveSessionID != next {
		t.Fatalf("generation packed as conflict: %#v %v", entry, err)
	}
	// Lose only the per-key overlay; the packed active identity must still route
	// correctly and a census must be able to restore that override.
	if err := os.Remove(qualifiedSessionIndexPath(s.home, key)); err != nil {
		t.Fatal(err)
	}
	if active, found, err := s.ArchiveSessionID(key); err != nil || !found || active != next {
		t.Fatalf("packed active route: %s %v %v", active, found, err)
	}
	if err := s.recoverRegistrationOwners(key, owners[key]); err != nil {
		t.Fatal(err)
	}
	if err := s.RemovePending(next); err != nil {
		t.Fatal(err)
	}
	request, _, err := s.LoadRequest(next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteRequest(next, request.Token); err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetSession(next, key); err != nil {
		t.Fatal(err)
	}
	completePackedGenerationCensus(t, s)
	if active, found, err := s.ArchiveSessionID(key); err != nil || found {
		t.Fatalf("retired tip revived ancestor: %s %v %v", active, found, err)
	}
	entry, found, err = s.readQualifiedIndex(key)
	if err != nil || !found || !entry.Absent {
		t.Fatalf("retired census absence: %#v %v", entry, err)
	}
}

func TestPackedCandidateRetainsFrozenGenerationParent(t *testing.T) {
	s, old, at, _ := packedGenerationFixture(t)
	if _, err := s.BeginGenerationRecovery(old.ArchiveSessionID, at, generationBuilder(at)); err != nil {
		t.Fatal(err)
	}
	childKey := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "queued-child"}
	candidate := SubagentCandidate{ArchiveSessionID: "queued-child-owner", NativeSessionID: childKey.NativeID, ParentArchiveSessionID: old.ArchiveSessionID, ParentNativeSessionID: old.NativeSessionID, Harness: old.Harness}
	if err := local.Write(s.subagentCandidatePath(candidate.ArchiveSessionID), candidate); err != nil {
		t.Fatal(err)
	}
	completePackedGenerationCensus(t, s)
	entry, found, err := s.readQualifiedIndex(childKey)
	if err != nil || !found || entry.ArchiveSessionID != candidate.ArchiveSessionID || entry.Reservation == "" {
		t.Fatalf("frozen parent lost candidate: %#v %v", entry, err)
	}
	if err := os.Remove(s.registrationPath(old.ArchiveSessionID)); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.readQualifiedIndex(childKey); found || !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		t.Fatalf("missing parent admitted candidate: %v %v", found, err)
	}
}

func completePackedGenerationCensus(t *testing.T, s *Store) sessionIndexMarker {
	t.Helper()
	lastStep := ""
	s.onIndexStep = func(step string) error { lastStep = step; return nil }
	defer func() { s.onIndexStep = nil }()
	if err := s.MarkSessionIndexRecoveryNeeded(); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		complete, err := s.RecoverSessionIndexScheduled(t.Context(), SessionIndexRecoverySlice)
		if err != nil {
			t.Fatalf("packed generation census after %s: %v", lastStep, err)
		}
		if complete {
			var marker sessionIndexMarker
			if err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &marker); err != nil || marker.Version != 2 || !marker.Complete {
				t.Fatalf("packed certificate: %#v %v", marker, err)
			}
			return marker
		}
	}
	t.Fatal("packed generation census did not converge")
	return sessionIndexMarker{}
}
