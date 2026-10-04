package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestPackedBatchCancellationSavesDurablePrefixAndCheckpointFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires ordinary filesystem permissions")
	}
	for _, writable := range []bool{true, false} {
		t.Run(strconv.FormatBool(writable), func(t *testing.T) {
			s, _, marker := packedOwnerFixture(t)
			cursor := sessionRecoveryCursor{Version: 2, Generation: marker.Generation, Revision: marker.PackedRevision, Inventory: marker.PackedInventory}
			if err := s.saveRecoveryCursor(&cursor); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			defer func() { _ = os.Chmod(s.home, 0700) }()
			syncs := 0
			s.onWriteSync = func() {
				syncs++
				if syncs == 2 {
					cancel()
					if !writable {
						if err := os.Chmod(s.home, 0500); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			var owners [packedSessionIndexShards]map[agentmeta.SessionKey][]string
			var children [packedSessionIndexShards][]SubagentCandidate
			complete, err := s.applyPackedShardPhase(ctx, &cursor, time.Now().Add(time.Second), marker, owners, children)
			if restoreErr := os.Chmod(s.home, 0700); restoreErr != nil {
				t.Fatal(restoreErr)
			}
			if complete || !errors.Is(err, context.Canceled) || cursor.Offset != 1 {
				t.Fatalf("complete=%v offset=%d err=%v", complete, cursor.Offset, err)
			}
			if errors.Is(err, os.ErrPermission) == writable || SessionIndexRecoveryInterrupted(err) != writable {
				t.Fatalf("checkpoint failure classification writable=%v: %v", writable, err)
			}
			var persisted sessionRecoveryCursor
			if err := local.Read(filepath.Join(s.home, sessionRecoveryCursorFile), &persisted); err != nil || !persisted.validChecksum() {
				t.Fatalf("persisted checkpoint: %#v %v", persisted, err)
			}
			want := 0
			if writable {
				want = 1
			}
			if persisted.Offset != want {
				t.Fatalf("persisted prefix=%d want=%d", persisted.Offset, want)
			}
			if _, err := s.readPackedSessionIndex("00", marker); err != nil {
				t.Fatal(err)
			}
			temps, err := filepath.Glob(filepath.Join(s.home, packedSessionIndexDir, ".pending-*"))
			if err != nil || len(temps) != 0 {
				t.Fatalf("staging not joined before checkpoint: %v %v", temps, err)
			}
		})
	}
}

func TestPackedBatchCancellationRetainsOnlyDurablePrefix(t *testing.T) {
	s, _, marker := packedOwnerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	syncs := 0
	s.onWriteSync = func() {
		syncs++
		// The first rename succeeded and its directory sync must still finish.
		if syncs == 2 {
			cancel()
		}
	}
	cursor := sessionRecoveryCursor{Version: 2}
	var owners [packedSessionIndexShards]map[agentmeta.SessionKey][]string
	var children [packedSessionIndexShards][]SubagentCandidate
	complete, err := s.applyPackedShardPhase(ctx, &cursor, time.Now().Add(time.Second), marker, owners, children)
	if complete || !errors.Is(err, context.Canceled) || cursor.Offset != 1 {
		t.Fatalf("complete=%v offset=%d err=%v", complete, cursor.Offset, err)
	}
	if _, err := s.readPackedSessionIndex("00", marker); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(packedIndexPath(s.home, "01")); !os.IsNotExist(err) {
		t.Fatalf("canceled shard published: %v", err)
	}
	temps, err := filepath.Glob(filepath.Join(s.home, packedSessionIndexDir, ".pending-*"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("staging not cleaned before return: %v %v", temps, err)
	}
}

func TestPackedBatchRechecksSecondSnapshotAfterFirstCommit(t *testing.T) {
	s, _, marker := packedOwnerFixture(t)
	syncs := 0
	second := packedIndexPath(s.home, "01")
	s.onWriteSync = func() {
		syncs++
		if syncs == 2 {
			if err := local.WriteBytes(second, []byte("concurrent replacement")); err != nil {
				t.Fatal(err)
			}
		}
	}
	cursor := sessionRecoveryCursor{Version: 2}
	var owners [packedSessionIndexShards]map[agentmeta.SessionKey][]string
	var children [packedSessionIndexShards][]SubagentCandidate
	complete, err := s.applyPackedShardPhase(context.Background(), &cursor, time.Now().Add(time.Second), marker, owners, children)
	if complete || !errors.Is(err, errIndexMoved) || cursor.Offset != 1 {
		t.Fatalf("complete=%v offset=%d err=%v", complete, cursor.Offset, err)
	}
	got, err := os.ReadFile(second)
	if err != nil || string(got) != "concurrent replacement" {
		t.Fatalf("replacement overwritten %q %v", got, err)
	}
}

func TestPackedBatchDoesNotAdvancePastFirstCommitFailure(t *testing.T) {
	s, _, marker := packedOwnerFixture(t)
	first := packedIndexPath(s.home, "00")
	syncs := 0
	s.onWriteSync = func() {
		syncs++
		if syncs == 1 {
			if err := os.Mkdir(first, 0700); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The directory prevents the first rename; the second independent file may
	// be durable, but it cannot authorize a contiguous checkpoint prefix.
	cursor := sessionRecoveryCursor{Version: 2}
	var owners [packedSessionIndexShards]map[agentmeta.SessionKey][]string
	var children [packedSessionIndexShards][]SubagentCandidate
	complete, err := s.applyPackedShardPhase(context.Background(), &cursor, time.Now().Add(time.Second), marker, owners, children)
	if complete || err == nil || cursor.Offset != 0 {
		t.Fatalf("complete=%v offset=%d err=%v", complete, cursor.Offset, err)
	}
}

func TestPackedBatchMembershipMutationFencesSecondCommit(t *testing.T) {
	s, _, marker := packedOwnerFixture(t)
	syncs := 0
	s.onWriteSync = func() {
		// Native staging and directory durability remain outside the short lock.
		unlock, err := local.NamedLock(s.home, sessionMembershipLock)
		if err != nil {
			t.Fatalf("sync seam held membership lock: %v", err)
		}
		unlock()
		syncs++
		if syncs == 2 {
			if err := local.WriteBytes(filepath.Join(s.home, sessionMembershipFile), []byte("changed-revision")); err != nil {
				t.Fatal(err)
			}
		}
	}
	cursor := sessionRecoveryCursor{Version: 2}
	var owners [packedSessionIndexShards]map[agentmeta.SessionKey][]string
	var children [packedSessionIndexShards][]SubagentCandidate
	complete, err := s.applyPackedShardPhase(context.Background(), &cursor, time.Now().Add(time.Second), marker, owners, children)
	if complete || !errors.Is(err, ErrSessionIndexRecoveryRequired) || cursor.Offset != 1 {
		t.Fatalf("complete=%v offset=%d err=%v", complete, cursor.Offset, err)
	}
	if _, err := os.Stat(packedIndexPath(s.home, "01")); !os.IsNotExist(err) {
		t.Fatalf("stale census shard published: %v", err)
	}
}

func TestPackedBatchJoinsSecondStagingFailure(t *testing.T) {
	s, _, marker := packedOwnerFixture(t)
	data, err := s.preparePackedShardSlice(context.Background(), marker, nil, nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(s.home, "blocked-parent")
	if err := os.WriteFile(blocked, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	errs := s.publishPackedBatch(context.Background(), marker, []packedPublication{
		{path: packedIndexPath(s.home, "00"), data: data},
		{path: filepath.Join(blocked, "01.idx"), data: data},
	})
	if errs[0] != nil || errs[1] == nil {
		t.Fatalf("independent durable results: %v", errs)
	}
	if _, err := s.readPackedSessionIndex("00", marker); err != nil {
		t.Fatal(err)
	}
	temps, err := filepath.Glob(filepath.Join(s.home, packedSessionIndexDir, ".pending-*"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("staging not joined/cleaned: %v %v", temps, err)
	}
}
