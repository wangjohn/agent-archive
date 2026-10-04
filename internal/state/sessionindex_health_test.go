package state

import (
	"context"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/local"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestRecoveryStatusReadsBoundedUnknownAndPendingEvidence(t *testing.T) {
	s := newTestStore(t)
	if health, err := s.SessionIndexRecoveryStatus(); err == nil || health.Complete || health.Pending || health.Phase != "unknown" {
		t.Fatalf("missing evidence: %#v %v", health, err)
	}
	seedRecoveryInventory(t, s, 2)
	if complete, err := s.RecoverSessionIndexScheduled(context.Background(), time.Nanosecond); complete || err != nil {
		t.Fatalf("pending: %v %v", complete, err)
	}
	if health, err := s.SessionIndexRecoveryStatus(); err != nil || !health.Pending || health.Phase != "registrations" || health.Complete {
		t.Fatalf("pending status: %#v %v", health, err)
	}
	if complete, err := s.RecoverSessionIndexScheduled(context.Background(), SessionIndexRecoverySlice); !complete || err != nil {
		t.Fatalf("complete: %v %v", complete, err)
	}
	if health, err := s.SessionIndexRecoveryStatus(); err != nil || !health.Complete || health.Pending {
		t.Fatalf("completed status: %#v %v", health, err)
	}
	if err := os.Remove(filepath.Join(s.home, sessionMembershipFile)); err != nil {
		t.Fatal(err)
	}
	if health, err := s.SessionIndexRecoveryStatus(); err == nil || health.Complete || health.Phase != "unknown" {
		t.Fatalf("missing completed fence: %#v %v", health, err)
	}
	if err := os.WriteFile(filepath.Join(s.home, sessionIndexMarkerFile), make([]byte, 5000), 0600); err != nil {
		t.Fatal(err)
	}
	if health, err := s.SessionIndexRecoveryStatus(); err == nil || health.Complete {
		t.Fatalf("oversized evidence: %#v %v", health, err)
	}
}

type recoveryFenceDamage string

const (
	fenceValid   recoveryFenceDamage = "valid"
	fenceMissing recoveryFenceDamage = "missing"
	fenceCorrupt recoveryFenceDamage = "corrupt"
	fenceChanged recoveryFenceDamage = "changed"
)

func TestPendingRecoveryHealthRequiresRecordedFenceOnly(t *testing.T) {
	for _, damage := range []recoveryFenceDamage{fenceValid, fenceMissing, fenceCorrupt, fenceChanged} {
		t.Run(string(damage), func(t *testing.T) {
			s := newTestStore(t)
			seedRecoveryInventory(t, s, 2)
			if complete, err := s.RecoverSessionIndexScheduled(context.Background(), time.Nanosecond); complete || err != nil {
				t.Fatalf("prepare pending: %v %v", complete, err)
			}
			path := filepath.Join(s.home, sessionMembershipFile)
			switch damage {
			case fenceValid:
			case fenceMissing:
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case fenceCorrupt:
				if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			case fenceChanged:
				if err := os.WriteFile(path, []byte(`{"version":1,"revision":"different-revision"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			health, err := s.SessionIndexRecoveryStatus()
			if damage == fenceValid {
				if err != nil || !health.Pending || health.Phase != "registrations" {
					t.Fatalf("valid pending: %#v %v", health, err)
				}
			} else if err == nil || health.Complete || health.Phase != "unknown" {
				t.Fatalf("damaged fence appeared trusted: %#v %v", health, err)
			}
		})
	}
	s := newTestStore(t)
	cursor, complete, err := s.prepareRecoveryCursor(context.Background())
	if err != nil || complete || cursor.Revision != "" {
		t.Fatalf("initial preparation: %#v %v %v", cursor, complete, err)
	}
	if err := s.saveRecoveryCursor(&cursor); err != nil {
		t.Fatal(err)
	}
	if health, err := s.SessionIndexRecoveryStatus(); err != nil || !health.Pending || health.Phase != "registrations" {
		t.Fatalf("first preparation without fence history: %#v %v", health, err)
	}
}

func TestBoundedRecoveryReaderAccessProbe(t *testing.T) {
	home := os.Getenv("AGENT_ARCHIVE_RECOVERY_ACCESS_PROBE_HOME")
	if home == "" {
		t.Skip("run-owned synthetic syscall probe")
	}
	probeID, err := strconv.ParseInt(home, 10, 64)
	if err != nil || probeID <= 0 {
		t.Fatal("access probe requires a positive synthetic directory token")
	}
	home = filepath.Join(string(filepath.Separator), "tmp", "u-reader-probe-"+strconv.FormatInt(probeID, 10))
	if os.Getenv("AGENT_ARCHIVE_RECOVERY_ACCESS_PROBE_PREPARE") == "1" {
		s, err := Open(home)
		if err != nil {
			t.Fatal(err)
		}
		seedRecoveryInventory(t, s, 2)
		if complete, err := s.RecoverSessionIndexScheduled(context.Background(), time.Nanosecond); complete || err != nil {
			t.Fatalf("pending: %v %v", complete, err)
		}
		return
	}
	health, err := OpenReadOnly(home).SessionIndexRecoveryStatus()
	if err != nil || !health.Pending || health.Phase != "registrations" {
		t.Fatalf("bounded recovery probe: %#v %v", health, err)
	}
}

type recoveryHealthDamage string

const (
	healthDamageMissing           recoveryHealthDamage = "missing"
	healthDamageCorrupt           recoveryHealthDamage = "corrupt"
	healthDamageFuture            recoveryHealthDamage = "future"
	healthDamageChecksum          recoveryHealthDamage = "checksum"
	healthDamageGeneration        recoveryHealthDamage = "generation"
	healthDamageRevision          recoveryHealthDamage = "revision"
	healthDamagePhase             recoveryHealthDamage = "phase"
	healthDamageOffset            recoveryHealthDamage = "offset"
	healthDamageEpoch             recoveryHealthDamage = "epoch"
	healthDamageInventory         recoveryHealthDamage = "inventory"
	healthDamageMembership        recoveryHealthDamage = "membership"
	healthDamageMissingMembership recoveryHealthDamage = "missing-membership"
	healthDamageCorruptMembership recoveryHealthDamage = "corrupt-membership"
	healthDamageFutureMembership  recoveryHealthDamage = "future-membership"
	healthDamageMissingAnchor     recoveryHealthDamage = "missing-anchor"
	healthDamageCorruptAnchor     recoveryHealthDamage = "corrupt-anchor"
)

func TestPackedRecoveryStatusScheduledEvidence(t *testing.T) {
	s := newTestStore(t)
	seedRecoveryInventory(t, s, 2)
	if err := s.MarkSessionIndexRecoveryNeeded(); err != nil {
		t.Fatal(err)
	}
	revision, err := s.ensureSessionMembershipRevision()
	if err != nil {
		t.Fatal(err)
	}
	// An existing supported packed marker selects the production packed scheduler
	// without changing its owner threshold or its slice allowance.
	if _, err := s.preparePackedSessionIndex(context.Background(), revision, "fixture-inventory"); err != nil {
		t.Fatal(err)
	}
	if complete, err := s.RecoverSessionIndexScheduled(context.Background(), time.Nanosecond); err != nil || complete {
		t.Fatalf("pending: %v %v", complete, err)
	}
	health, err := OpenReadOnly(s.home).SessionIndexRecoveryStatus()
	if err != nil || !health.Pending || health.Complete || health.Phase != "shards" {
		t.Fatalf("packed pending: %#v %v", health, err)
	}
	var cursor sessionRecoveryCursor
	if err := local.Read(filepath.Join(s.home, sessionRecoveryCursorFile), &cursor); err != nil || cursor.Version != 2 || !cursor.validChecksum() {
		t.Fatalf("persisted packed cursor: %#v %v", cursor, err)
	}
	for _, damage := range []recoveryHealthDamage{healthDamageMissing, healthDamageCorrupt, healthDamageFuture, healthDamageChecksum, healthDamageGeneration, healthDamageRevision, healthDamagePhase, healthDamageOffset} {
		t.Run("pending-"+string(damage), func(t *testing.T) {
			damaged := cursor
			path := filepath.Join(s.home, sessionRecoveryCursorFile)
			switch damage {
			case healthDamageMissing:
				err = os.Remove(path)
			case healthDamageCorrupt:
				err = os.WriteFile(path, []byte("{"), 0600)
			case healthDamageFuture:
				damaged.Version = 3
				err = s.saveRecoveryCursor(&damaged)
			case healthDamageChecksum:
				damaged.Checksum = "invalid"
				err = local.Write(path, damaged)
			case healthDamageGeneration:
				damaged.Generation = "changed"
				err = s.saveRecoveryCursor(&damaged)
			case healthDamageRevision:
				damaged.Revision = "changed"
				err = s.saveRecoveryCursor(&damaged)
			case healthDamagePhase:
				damaged.Phase = 3
				err = s.saveRecoveryCursor(&damaged)
			case healthDamageOffset:
				damaged.Offset = packedSessionIndexShards + 1
				err = s.saveRecoveryCursor(&damaged)
			}
			if err != nil {
				t.Fatal(err)
			}
			if health, err := s.SessionIndexRecoveryStatus(); err == nil || health.Complete || health.Phase != "unknown" {
				t.Fatalf("damaged cursor: %#v %v", health, err)
			}
			if err := s.saveRecoveryCursor(&cursor); err != nil {
				t.Fatal(err)
			}
		})
	}
	for phase, name := range []string{"shards", "fallback-owners-and-candidates", "requested-misses"} {
		cursor.Phase = phase
		if err := s.saveRecoveryCursor(&cursor); err != nil {
			t.Fatal(err)
		}
		if health, err := s.SessionIndexRecoveryStatus(); err != nil || health.Phase != name || !health.Pending {
			t.Fatalf("phase %d: %#v %v", phase, health, err)
		}
	}
	cursor.Phase = 0
	if err := s.saveRecoveryCursor(&cursor); err != nil {
		t.Fatal(err)
	}
	complete := false
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
		t.Fatal("packed recovery did not complete")
	}
	reopened, err := Open(s.home)
	if err != nil {
		t.Fatal(err)
	}
	health, err = reopened.SessionIndexRecoveryStatus()
	if err != nil || !health.Complete || health.Pending || health.Phase != "complete" {
		t.Fatalf("packed completion: %#v %v", health, err)
	}
	data, err := json.Marshal(health)
	if err != nil || string(data) != `{"complete":true,"pending":false,"phase":"complete"}` {
		t.Fatalf("content-free JSON: %s %v", data, err)
	}
	var marker sessionIndexMarker
	if err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &marker); err != nil || marker.Version != 2 || !marker.Complete || !marker.MembershipFenced {
		t.Fatalf("packed certificate: %#v %v", marker, err)
	}
	anchorPath := filepath.Join(s.home, "sessions-v1", packedOverlaySentinel)
	anchor, err := os.ReadFile(anchorPath)
	if err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(s.home, sessionIndexMarkerFile)
	for _, damage := range []recoveryHealthDamage{healthDamageMissing, healthDamageCorrupt, healthDamageFuture, healthDamageEpoch, healthDamageRevision, healthDamageInventory, healthDamageMembership, healthDamageMissingMembership, healthDamageCorruptMembership, healthDamageFutureMembership, healthDamageMissingAnchor, healthDamageCorruptAnchor} {
		t.Run(string(damage), func(t *testing.T) {
			damaged := marker
			switch damage {
			case healthDamageMissing:
				err = os.Remove(markerPath)
			case healthDamageCorrupt:
				err = os.WriteFile(markerPath, []byte("{"), 0600)
			case healthDamageFuture:
				damaged.Version = 3
				err = local.Write(markerPath, damaged)
			case healthDamageEpoch:
				damaged.PackedEpoch = ""
				err = local.Write(markerPath, damaged)
			case healthDamageRevision:
				damaged.PackedRevision = "changed"
				err = local.Write(markerPath, damaged)
			case healthDamageInventory:
				damaged.PackedInventory = ""
				err = local.Write(markerPath, damaged)
			case healthDamageMissingMembership:
				err = os.Remove(filepath.Join(s.home, sessionMembershipFile))
			case healthDamageCorruptMembership:
				err = os.WriteFile(filepath.Join(s.home, sessionMembershipFile), []byte("{"), 0600)
			case healthDamageFutureMembership:
				err = local.Write(filepath.Join(s.home, sessionMembershipFile), sessionMembershipRevision{Version: 2, Revision: revision})
			case healthDamageMissingAnchor:
				err = os.Remove(anchorPath)
			case healthDamageCorruptAnchor:
				err = os.WriteFile(anchorPath, []byte("changed"), 0600)
			case healthDamageMembership:
				err = local.Write(filepath.Join(s.home, sessionMembershipFile), sessionMembershipRevision{Version: 1, Revision: "changed"})
			}
			if err != nil {
				t.Fatal(err)
			}
			if health, err := s.SessionIndexRecoveryStatus(); err == nil || health.Complete || health.Phase != "unknown" {
				t.Fatalf("damaged certificate: %#v %v", health, err)
			}
			if err := os.WriteFile(anchorPath, anchor, 0600); err != nil {
				t.Fatal(err)
			}
			if err := local.Write(markerPath, marker); err != nil {
				t.Fatal(err)
			}
			if err := local.Write(filepath.Join(s.home, sessionMembershipFile), sessionMembershipRevision{Version: 1, Revision: revision}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
