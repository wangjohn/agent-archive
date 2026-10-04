package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"os"
	"path/filepath"
	"testing"
)

func packedLifecycleFixture(t *testing.T) (*Store, string) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := range packedSessionIndexThreshold {
		key := migrationRegistrationKey(i)
		reg := migrationRegistration(key, fmt.Sprintf("lifecycle-owner-%06d", i))
		data, err := json.Marshal(reg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.registrationPath(reg.ArchiveSessionID), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkSessionIndexRecoveryNeeded(); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice)
		if err != nil {
			t.Fatal(err)
		}
		if complete {
			return s, "lifecycle-owner-000000"
		}
	}
	t.Fatal("fixture did not complete original twenty slices")
	return nil, ""
}

func TestPackedLifecycleOverlayDirectoryLoss(t *testing.T) {
	testPackedLifecycleLoss(t, false)
}
func TestPackedLifecycleAllJSONLoss(t *testing.T) { testPackedLifecycleLoss(t, true) }
func testPackedLifecycleLoss(t *testing.T, jsonOnly bool) {
	s, old := packedLifecycleFixture(t)
	mixed := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "mixed-before-forget"}
	mixedID, created, err := s.EnsureArchiveSessionID(mixed)
	if err != nil || !created {
		t.Fatalf("mixed fresh session: %v %v", created, err)
	}
	if err := s.SaveRegistration(migrationRegistration(mixed, mixedID)); err != nil {
		t.Fatal(err)
	}

	key := migrationRegistrationKey(0)
	if err := s.ForgetSession(old, key); err != nil {
		t.Fatal(err)
	}
	replacement, created, err := s.EnsureArchiveSessionID(key)
	if err != nil || !created {
		t.Fatalf("replacement %v %v", created, err)
	}
	if err := s.SaveRegistration(migrationRegistration(key, replacement)); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"sessions-v1", "sessions"} {
		if jsonOnly {
			files, err := filepath.Glob(filepath.Join(s.home, dir, "*.json"))
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range files {
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
			}
		} else if err := os.RemoveAll(filepath.Join(s.home, dir)); err != nil {
			t.Fatal(err)
		}
	}
	s, err = Open(s.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnsureArchiveSessionID(key); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		t.Fatalf("lost overlay permits allocation: %v", err)
	}
	for range 20 {
		complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice)
		if err != nil {
			t.Fatal(err)
		}
		if !complete {
			continue
		}
		id, found, err := s.ArchiveSessionID(key)
		if err != nil || !found || id != replacement {
			t.Fatalf("replacement invisible: %q %v %v", id, found, err)
		}
		return
	}
	t.Fatal("lost overlay did not recover")
}

func TestPackedLifecycleExpiryRemovesMetadata(t *testing.T) {
	s, old := packedLifecycleFixture(t)
	for i := 0; i < 2; i++ {
		freshKey := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: fmt.Sprintf("fresh-sequential-%d", i)}
		id, created, err := s.EnsureArchiveSessionID(freshKey)
		if err != nil || !created {
			t.Fatalf("healthy fresh session %d: %v %v", i, created, err)
		}
		if err := s.SaveRegistration(migrationRegistration(freshKey, id)); err != nil {
			t.Fatal(err)
		}
	}

	key := migrationRegistrationKey(0)
	if err := s.ForgetSession(old, key); err != nil {
		t.Fatal(err)
	}
	var err error
	s, err = Open(s.home)
	if err != nil {
		t.Fatal(err)
	}
	var marker sessionIndexMarker
	if err := readRecoveryJSON(filepath.Join(s.home, sessionIndexMarkerFile), &marker); err != nil {
		t.Fatal(err)
	}
	index, err := s.readPackedSessionIndex(packedIndexHash(key)[:2], marker)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := index.Entries[packedIndexHash(key)]; found {
		t.Fatal("expired identity metadata retained")
	}
}

func migrationRegistrationKey(i int) agentmeta.SessionKey {
	return agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: fmt.Sprintf("lifecycle-native-%06d", i)}
}

func TestPackedLinkedCandidateExpiryMetadata(t *testing.T) {
	s, parent, marker := packedOwnerFixture(t)
	child := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "expiry-child"}
	candidate := SubagentCandidate{ArchiveSessionID: "expiry-child-owner", NativeSessionID: child.NativeID, ParentArchiveSessionID: "packed-owner", ParentNativeSessionID: parent.NativeID, Harness: archive.Harness{Name: "codex"}}
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
	marker.Complete = true
	if err := local.Write(filepath.Join(s.home, sessionIndexMarkerFile), marker); err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetSession("packed-owner", parent); err != nil {
		t.Fatal(err)
	}
	index, err := s.readPackedSessionIndex(packedIndexHash(child)[:2], marker)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := index.Entries[packedIndexHash(child)]; found {
		t.Fatal("linked expired candidate identity retained")
	}
}

func TestPackedSkewedShardDefersNativeOwnerWrites(t *testing.T) {
	s, _, marker := packedOwnerFixture(t)
	marker.Complete = false
	if err := local.Write(filepath.Join(s.home, sessionIndexMarkerFile), marker); err != nil {
		t.Fatal(err)
	}
	owners := map[agentmeta.SessionKey][]string{}
	for i := 0; len(owners) < 1025; i++ {
		key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: fmt.Sprintf("skewed-native-%06d", i)}
		if packedIndexHash(key)[:2] != "00" {
			continue
		}
		id := fmt.Sprintf("skewed-owner-%06d", i)
		data, err := json.Marshal(migrationRegistration(key, id))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.registrationPath(id), data, 0600); err != nil {
			t.Fatal(err)
		}
		owners[key] = []string{id}
	}
	// The fixture bypasses live producers, so preserve the marker's census
	// revision; the application operation must publish only an empty fallback.
	if err := s.recoverPackedShard(context.Background(), "00", marker, owners, nil); err != nil {
		t.Fatal(err)
	}
	index, err := s.readPackedSessionIndex("00", marker)
	if err != nil {
		t.Fatal(err)
	}
	if !index.Fallback || len(index.Entries) != 0 {
		t.Fatal("skewed shard not deferred to individually checkpointed fallback phase")
	}
	for key := range owners {
		if _, err := os.Stat(qualifiedSessionIndexPath(s.home, key)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owner write escaped shard application: %v", err)
		}
	}
}
