package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/local"
)

func packedFreshKeyInShard(shard, prefix string) agentmeta.SessionKey {
	for i := 0; ; i++ {
		key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: fmt.Sprintf("%s-%d", prefix, i)}
		if packedIndexHash(key)[:2] == shard {
			return key
		}
	}
}

func TestPackedAnchorDamageFailsClosedWithUnrelatedOverride(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(strconv.FormatBool(corrupt), func(t *testing.T) {
			s, owner, marker := packedOwnerFixture(t)
			shard := packedIndexHash(owner)[:2]
			for i := range 2 {
				key := packedFreshKeyInShard(shard, fmt.Sprintf("anchor-damage-%d", i))
				id, _, err := s.EnsureArchiveSessionID(key)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.SaveRegistration(migrationRegistration(key, id)); err != nil {
					t.Fatal(err)
				}
			}
			data, err := os.ReadFile(filepath.Join(s.home, "sessions-v1", packedOverlaySentinel))
			if err != nil {
				t.Fatal(err)
			}
			anchor, valid := packedOverlayAnchor(string(data), marker)
			if !valid || anchor == "" {
				t.Fatal("missing actual anchor")
			}
			path := filepath.Join(s.home, "sessions-v1", anchor+".json")
			if corrupt {
				err = os.WriteFile(path, []byte("{"), 0600)
			} else {
				err = os.Remove(path)
			}
			if err != nil {
				t.Fatal(err)
			}
			s, err = Open(s.home)
			if err != nil {
				t.Fatal(err)
			}
			fresh := packedFreshKeyInShard(shard, "fresh-after-anchor-damage")
			if _, _, err := s.ForHook().EnsureArchiveSessionID(fresh); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
				t.Fatalf("damaged anchor with surviving unrelated override: %v", err)
			}
		})
	}
}

func TestPackedAnchorRetirementPreservesConcurrentReservation(t *testing.T) {
	s, owner, marker := packedOwnerFixture(t)
	shard := packedIndexHash(owner)[:2]
	old := packedFreshKeyInShard(shard, "retirement-old")
	if _, _, err := s.EnsureArchiveSessionID(old); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(qualifiedSessionIndexPath(s.home, old)); err != nil {
		t.Fatal(err)
	}
	next := packedFreshKeyInShard(shard, "retirement-concurrent")
	fired := false
	s.onWriteSync = func() {
		if fired {
			return
		}
		fired = true
		for _, name := range []string{"hooks.lock", sessionMembershipLock} {
			unlock, err := local.NamedLock(s.home, name)
			if err != nil {
				t.Fatalf("native staging held %s: %v", name, err)
			}
			unlock()
		}
		entry := indexEntry(next, "concurrent-reservation-owner")
		entry.Reservation = "reservation-token"
		if err := s.writeIndexUnderRequestLock(entry.ArchiveSessionID, qualifiedSessionIndexPath(s.home, next), nil, func(fileSnapshot) (any, bool, error) { return entry, true, nil }); err != nil {
			t.Fatal(err)
		}
	}
	err := s.retirePackedOverlayAnchor(marker, old)
	if !fired || !errors.Is(err, errIndexMoved) {
		t.Fatalf("retirement failed to fence changed anchor: fired=%v err=%v", fired, err)
	}
	var entry qualifiedSessionIndexEntry
	if err := local.Read(qualifiedSessionIndexPath(s.home, next), &entry); err != nil || entry.Reservation == "" {
		t.Fatalf("concurrent reservation lost: %+v %v", entry, err)
	}
	if !s.packedOverlaysHealthy(marker) {
		t.Fatal("concurrent reservation anchor overwritten by empty retirement")
	}
}

func TestPackedLastOverrideExpiryAllowsUnrelatedFreshHook(t *testing.T) {
	s, _ := packedLifecycleFixture(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "only-physical-override"}
	id, created, err := s.ForHook().EnsureArchiveSessionID(key)
	if err != nil || !created {
		t.Fatalf("first fresh: %v %v", created, err)
	}
	if err := s.SaveRegistration(migrationRegistration(key, id)); err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetSession(id, key); err != nil {
		t.Fatal(err)
	}
	s, err = Open(s.home)
	if err != nil {
		t.Fatal(err)
	}
	next := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "unrelated-after-last-expiry"}
	_, created, err = s.ForHook().EnsureArchiveSessionID(next)
	if err != nil || !created {
		t.Fatalf("unrelated fresh after successful last-override expiry: %v %v", created, err)
	}
}

func TestPackedFreshHookDoesNotEnumeratePhysicalNamespace(t *testing.T) {
	s, _, _ := packedOwnerFixture(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "retained-physical-anchor"}
	// Fixture contains one shard; use a key in that shard for both fresh writes.
	shard := packedIndexHash(agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "packed-native"})[:2]
	for i := 0; packedIndexHash(key)[:2] != shard; i++ {
		key.NativeID = fmt.Sprintf("retained-anchor-%d", i)
	}
	id, _, err := s.EnsureArchiveSessionID(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRegistration(migrationRegistration(key, id)); err != nil {
		t.Fatal(err)
	}
	for i := range 2048 {
		if err := os.WriteFile(filepath.Join(s.home, "sessions-v1", fmt.Sprintf("foreign-%06d.removed", i)), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, err = Open(s.home)
	if err != nil {
		t.Fatal(err)
	}
	probes := 0
	s.onPackedEnumeration = func() { probes++ }
	next := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "new-bounded-hook"}
	for i := 0; packedIndexHash(next)[:2] != shard || next == key; i++ {
		next.NativeID = fmt.Sprintf("new-bounded-hook-%d", i)
	}
	_, created, err := s.ForHook().EnsureArchiveSessionID(next)
	if err != nil || !created {
		t.Fatalf("fresh hook: %v %v", created, err)
	}
	if probes != 0 {
		t.Fatalf("ordinary hook enumerated physical namespace %d times", probes)
	}
}
