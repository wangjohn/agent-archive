package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

func packedOwnerFixture(tb testing.TB) (*Store, agentmeta.SessionKey, sessionIndexMarker) {
	tb.Helper()
	s, err := openTestStore(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "packed-native"}
	reg := migrationRegistration(key, "packed-owner")
	data, err := json.Marshal(reg)
	if err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(s.registrationPath(reg.ArchiveSessionID), data, 0600); err != nil {
		tb.Fatal(err)
	}
	if err := s.MarkSessionIndexRecoveryNeeded(); err != nil {
		tb.Fatal(err)
	}
	revision, err := s.ensureSessionMembershipRevision()
	if err != nil {
		tb.Fatal(err)
	}
	marker, err := s.preparePackedSessionIndex(context.Background(), revision, "test-inventory")
	if err != nil {
		tb.Fatal(err)
	}
	if err := s.recoverPackedShard(context.Background(), packedIndexHash(key)[:2], marker, map[agentmeta.SessionKey][]string{key: {reg.ArchiveSessionID}}, nil); err != nil {
		tb.Fatal(err)
	}
	if err := s.ensurePackedOverlayDirectory(marker); err != nil {
		tb.Fatal(err)
	}
	marker.Complete = true
	if err := local.Write(filepath.Join(s.home, sessionIndexMarkerFile), marker); err != nil {
		tb.Fatal(err)
	}
	return s, key, marker
}

func TestPackedOwnerLookupOverlayAndForget(t *testing.T) {
	s, key, _ := packedOwnerFixture(t)
	id, found, err := s.ArchiveSessionID(key)
	if err != nil || !found || id != "packed-owner" {
		t.Fatalf("lookup=(%q,%v,%v)", id, found, err)
	}
	if err := s.finishSessionIndex(key, id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(qualifiedSessionIndexPath(s.home, key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unchanged owner materialized: %v", err)
	}
	if err := s.ForgetSession(id, key); err != nil {
		t.Fatal(err)
	}
	fresh, created, err := s.EnsureArchiveSessionID(key)
	if err != nil || !created || fresh == id {
		t.Fatalf("forgotten packed owner reused: %q %v %v", fresh, created, err)
	}
	reg := migrationRegistration(key, fresh)
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.ArchiveSessionID(key)
	if err != nil || !found || got != fresh {
		t.Fatalf("override lost: %q %v %v", got, found, err)
	}
	if err := os.WriteFile(qualifiedSessionIndexPath(s.home, key), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.readQualifiedIndex(key); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		t.Fatalf("damaged override fell back: %v", err)
	}
}

func TestPackedLossCorruptionAndPredecessorFence(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(strconv.FormatBool(corrupt), func(t *testing.T) {
			s, key, marker := packedOwnerFixture(t)
			// The predecessor only permits misses under a complete Version1 marker.
			if marker.Version == 1 || !marker.Complete {
				t.Fatalf("predecessor miss fence lost: %#v", marker)
			}
			path := packedIndexPath(s.home, packedIndexHash(key)[:2])
			if corrupt {
				if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.readQualifiedIndex(key); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
				t.Fatalf("lost shard accepted: %v", err)
			}
		})
	}
	s, key, marker := packedOwnerFixture(t)
	marker.Version = 1
	if err := local.Write(filepath.Join(s.home, sessionIndexMarkerFile), marker); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.readQualifiedIndex(key); err != nil || found {
		t.Fatalf("predecessor-owned namespace reactivated packed leftovers: %v %v", found, err)
	}
}

func TestPackedCandidateAdmissionAndParentRemoval(t *testing.T) {
	for _, admit := range []bool{false, true} {
		t.Run(strconv.FormatBool(admit), func(t *testing.T) {
			s, parent, marker := packedOwnerFixture(t)
			child := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "packed-child"}
			candidate := SubagentCandidate{ArchiveSessionID: "packed-child-owner", NativeSessionID: child.NativeID, ParentArchiveSessionID: "packed-owner", ParentNativeSessionID: parent.NativeID, Harness: archive.Harness{Name: "codex"}}
			if err := local.Write(s.subagentCandidatePath(candidate.ArchiveSessionID), candidate); err != nil {
				t.Fatal(err)
			}
			marker.Complete = false
			if err := local.Write(filepath.Join(s.home, sessionIndexMarkerFile), marker); err != nil {
				t.Fatal(err)
			}
			if err := s.recoverPackedShard(context.Background(), packedIndexHash(child)[:2], marker, nil, []SubagentCandidate{candidate}); err != nil {
				t.Fatal(err)
			}
			entry, found, err := s.readQualifiedIndex(child)
			if err != nil || !found || entry.Reservation == "" {
				t.Fatalf("reservation lost: %#v %v %v", entry, found, err)
			}
			if admit {
				data, _ := json.Marshal(migrationRegistration(child, candidate.ArchiveSessionID))
				if err := os.WriteFile(s.registrationPath(candidate.ArchiveSessionID), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Remove(s.subagentCandidatePath(candidate.ArchiveSessionID)); err != nil {
				t.Fatal(err)
			}
			entry, found, err = s.readQualifiedIndex(child)
			if admit {
				if err != nil || !found || entry.Reservation != "" {
					t.Fatalf("admitted owner lost: %#v %v %v", entry, found, err)
				}
			} else if !errors.Is(err, ErrSessionIndexRecoveryRequired) {
				t.Fatalf("removed candidate revived: %#v %v %v", entry, found, err)
			}
		})
	}
}

func TestPackedOversizedIdentityFallsBack(t *testing.T) {
	s, key, marker := packedOwnerFixture(t)
	marker.Complete = false
	if err := local.Write(filepath.Join(s.home, sessionIndexMarkerFile), marker); err != nil {
		t.Fatal(err)
	}
	huge := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: strings.Repeat("x", packedSessionIndexMaxBytes)}
	reg := migrationRegistration(huge, "large-owner")
	data, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.registrationPath(reg.ArchiveSessionID), data, 0600); err != nil {
		t.Fatal(err)
	}
	shard := packedIndexHash(huge)[:2]
	if err := s.recoverPackedShard(context.Background(), shard, marker, map[agentmeta.SessionKey][]string{huge: {reg.ArchiveSessionID}}, nil); err != nil {
		t.Fatal(err)
	}
	// Scheduled fallback applies ordinary owners in individually checkpointed units.
	for attempt := range 20 {
		complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice)
		if err != nil {
			t.Fatal(err)
		}
		if complete {
			break
		}
		if attempt == 19 {
			t.Fatal("oversized scheduled fallback did not complete")
		}
	}
	entry, found, err := s.readQualifiedIndex(huge)
	if err != nil || !found || entry.ArchiveSessionID != reg.ArchiveSessionID {
		t.Fatalf("fallback failed: %v %v", found, err)
	}
	_ = key
}

func TestPackedScheduledResumeRepairAndCertification(t *testing.T) {
	s, err := openTestStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := range packedSessionIndexThreshold {
		nativeID := fmt.Sprintf("large-native-%06d", i)
		id := fmt.Sprintf("large-owner-%06d", i)
		if i == 0 {
			nativeID = strings.Repeat("large-read-ahead-", 8192)
		}
		key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: nativeID}
		data, err := json.Marshal(migrationRegistration(key, id))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.registrationPath(id), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkSessionIndexRecoveryNeeded(); err != nil {
		t.Fatal(err)
	}
	// A tiny allowance establishes a real durable cursor before reopening.
	complete, err := s.RecoverSessionIndexScheduled(context.Background(), time.Nanosecond)
	if err != nil || complete {
		t.Fatalf("initial=%v %v", complete, err)
	}
	var cursor sessionRecoveryCursor
	if err := local.Read(filepath.Join(s.home, sessionRecoveryCursorFile), &cursor); err != nil || !cursor.validChecksum() || cursor.Version != 2 {
		t.Fatalf("cursor=%#v %v", cursor, err)
	}
	s, err = Open(s.home)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := range 20 {
		complete, err = s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice)
		if err != nil {
			t.Fatal(err)
		}
		if complete {
			t.Logf("complete after %d calls", attempt+1)
			break
		}
	}
	if !complete {
		t.Fatal("large packed recovery did not converge")
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "large-native-000005"}
	id, found, err := s.ArchiveSessionID(key)
	if err != nil || !found || id != "large-owner-000005" {
		t.Fatalf("lookup=%q %v %v", id, found, err)
	}
	// A later exact-key request cannot be hidden by the retained packed offset.
	if err := os.WriteFile(qualifiedSessionIndexPath(s.home, key), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestSessionIndexRecovery(key); err != nil {
		t.Fatal(err)
	}
	unknown := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "large-requested-unknown"}
	if err := s.RequestSessionIndexRecovery(unknown); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		complete, err = s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice)
		if err != nil {
			t.Fatal(err)
		}
		if complete {
			break
		}
	}
	if !complete {
		t.Fatal("requested repair did not converge")
	}
	absent, err := s.SessionIndexAbsent(unknown)
	if err != nil || !absent {
		t.Fatalf("absence=%v %v", absent, err)
	}
	id, found, err = s.ArchiveSessionID(key)
	if err != nil || !found || id != "large-owner-000005" {
		t.Fatalf("repaired lookup=%q %v %v", id, found, err)
	}
	// Lost completed shards are rebuilt instead of retaining the certificate.
	if err := os.Remove(packedIndexPath(s.home, packedIndexHash(key)[:2])); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		complete, err = s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice)
		if err != nil {
			t.Fatal(err)
		}
		if complete {
			break
		}
	}
	if !complete {
		t.Fatal("lost completed shard did not converge")
	}
	if _, found, err := s.ArchiveSessionID(key); err != nil || !found {
		t.Fatalf("lost shard not restored: %v %v", found, err)
	}
}

func TestPackedCandidateParentRemovalAndSnapshotRace(t *testing.T) {
	s, parent, marker := packedOwnerFixture(t)
	child := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "packed-race-child"}
	candidate := SubagentCandidate{ArchiveSessionID: "packed-race-child-owner", NativeSessionID: child.NativeID, ParentArchiveSessionID: "packed-owner", ParentNativeSessionID: parent.NativeID, Harness: archive.Harness{Name: "codex"}}
	if err := local.Write(s.subagentCandidatePath(candidate.ArchiveSessionID), candidate); err != nil {
		t.Fatal(err)
	}
	marker.Complete = false
	if err := local.Write(filepath.Join(s.home, sessionIndexMarkerFile), marker); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverPackedShard(context.Background(), packedIndexHash(child)[:2], marker, nil, []SubagentCandidate{candidate}); err != nil {
		t.Fatal(err)
	}
	fired := false
	s.onIndexSync = func() {
		if fired {
			return
		}
		fired = true
		if err := os.Remove(s.registrationPath(candidate.ParentArchiveSessionID)); err != nil {
			t.Fatal(err)
		}
	}
	err := s.writeIndexUnderRequestLock(candidate.ArchiveSessionID, qualifiedSessionIndexPath(s.home, child), nil, func(current fileSnapshot) (any, bool, error) {
		if !current.found {
			t.Fatal("packed reservation invisible before staging")
		}
		return indexEntry(child, candidate.ArchiveSessionID), true, nil
	})
	if !errors.Is(err, ErrSessionIndexRecoveryRequired) || !fired {
		t.Fatalf("parentremoved during stagedwrite: %v fired=%v", err, fired)
	}
	if _, err := os.Stat(qualifiedSessionIndexPath(s.home, child)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale reservation committed: %v", err)
	}
	if _, _, err := s.readQualifiedIndex(child); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		t.Fatalf("removed parent revived: %v", err)
	}
}

func (s *Store) recoverPackedShard(ctx context.Context, shard string, marker sessionIndexMarker, owners map[agentmeta.SessionKey][]string, candidates []SubagentCandidate) error {
	data, err := s.preparePackedShardSlice(ctx, marker, owners, candidates, time.Time{})
	if err != nil {
		return err
	}
	path := packedIndexPath(s.home, shard)
	before, err := readSnapshot(path)
	if err != nil {
		return err
	}
	return s.publishPackedBatch(ctx, marker, []packedPublication{{path: path, before: before, data: data}})[0]
}
