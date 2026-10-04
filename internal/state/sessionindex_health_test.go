package state

import (
	"context"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/local"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

type pendingHealthDamage string

const (
	pendingDamageMissing    pendingHealthDamage = "missing"
	pendingDamageCorrupt    pendingHealthDamage = "corrupt"
	pendingDamageFuture     pendingHealthDamage = "future"
	pendingDamageChecksum   pendingHealthDamage = "checksum"
	pendingDamageGeneration pendingHealthDamage = "generation"
	pendingDamageRevision   pendingHealthDamage = "revision"
	pendingDamagePhase      pendingHealthDamage = "phase"
	pendingDamageOffset     pendingHealthDamage = "offset"
)

type certificateHealthDamage string

const (
	certificateDamageMissing           certificateHealthDamage = "missing"
	certificateDamageCorrupt           certificateHealthDamage = "corrupt"
	certificateDamageFuture            certificateHealthDamage = "future"
	certificateDamageEpoch             certificateHealthDamage = "epoch"
	certificateDamageRevision          certificateHealthDamage = "revision"
	certificateDamageInventory         certificateHealthDamage = "inventory"
	certificateDamageMembership        certificateHealthDamage = "membership"
	certificateDamageMissingMembership certificateHealthDamage = "missing-membership"
	certificateDamageCorruptMembership certificateHealthDamage = "corrupt-membership"
	certificateDamageFutureMembership  certificateHealthDamage = "future-membership"
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
	if err != nil || !health.Pending || health.Complete || health.Phase != "packed-shards" {
		t.Fatalf("packed pending: %#v %v", health, err)
	}
	var cursor sessionRecoveryCursor
	if err := local.Read(filepath.Join(s.home, sessionRecoveryCursorFile), &cursor); err != nil || cursor.Version != 2 || !cursor.validChecksum() {
		t.Fatalf("persisted packed cursor: %#v %v", cursor, err)
	}
	for _, damage := range []pendingHealthDamage{pendingDamageMissing, pendingDamageCorrupt, pendingDamageFuture, pendingDamageChecksum, pendingDamageGeneration, pendingDamageRevision, pendingDamagePhase, pendingDamageOffset} {
		t.Run("pending-"+string(damage), func(t *testing.T) {
			damaged := cursor
			path := filepath.Join(s.home, sessionRecoveryCursorFile)
			switch damage {
			case pendingDamageMissing:
				err = os.Remove(path)
			case pendingDamageCorrupt:
				err = os.WriteFile(path, []byte("{"), 0600)
			case pendingDamageFuture:
				damaged.Version = 3
				err = s.saveRecoveryCursor(&damaged)
			case pendingDamageChecksum:
				damaged.Checksum = "invalid"
				err = local.Write(path, damaged)
			case pendingDamageGeneration:
				damaged.Generation = "changed"
				err = s.saveRecoveryCursor(&damaged)
			case pendingDamageRevision:
				damaged.Revision = "changed"
				err = s.saveRecoveryCursor(&damaged)
			case pendingDamagePhase:
				damaged.Phase = 3
				err = s.saveRecoveryCursor(&damaged)
			case pendingDamageOffset:
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
	for phase, name := range []string{"packed-shards", "packed-fallback", "requested-misses"} {
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
	markerPath := filepath.Join(s.home, sessionIndexMarkerFile)
	for _, damage := range []certificateHealthDamage{certificateDamageMissing, certificateDamageCorrupt, certificateDamageFuture, certificateDamageEpoch, certificateDamageRevision, certificateDamageInventory, certificateDamageMembership, certificateDamageMissingMembership, certificateDamageCorruptMembership, certificateDamageFutureMembership} {
		t.Run(string(damage), func(t *testing.T) {
			damaged := marker
			switch damage {
			case certificateDamageMissing:
				err = os.Remove(markerPath)
			case certificateDamageCorrupt:
				err = os.WriteFile(markerPath, []byte("{"), 0600)
			case certificateDamageFuture:
				damaged.Version = 3
				err = local.Write(markerPath, damaged)
			case certificateDamageEpoch:
				damaged.PackedEpoch = ""
				err = local.Write(markerPath, damaged)
			case certificateDamageRevision:
				damaged.PackedRevision = "changed"
				err = local.Write(markerPath, damaged)
			case certificateDamageInventory:
				damaged.PackedInventory = ""
				err = local.Write(markerPath, damaged)
			case certificateDamageMissingMembership:
				err = os.Remove(filepath.Join(s.home, sessionMembershipFile))
			case certificateDamageCorruptMembership:
				err = os.WriteFile(filepath.Join(s.home, sessionMembershipFile), []byte("{"), 0600)
			case certificateDamageFutureMembership:
				err = local.Write(filepath.Join(s.home, sessionMembershipFile), sessionMembershipRevision{Version: 2, Revision: revision})
			case certificateDamageMembership:
				err = local.Write(filepath.Join(s.home, sessionMembershipFile), sessionMembershipRevision{Version: 1, Revision: "changed"})
			}
			if err != nil {
				t.Fatal(err)
			}
			if health, err := s.SessionIndexRecoveryStatus(); err == nil || health.Complete || health.Phase != "unknown" {
				t.Fatalf("damaged certificate: %#v %v", health, err)
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

type packedHealthFieldDamage string

const (
	packedFieldMarkerVersion    packedHealthFieldDamage = "marker-version"
	packedFieldCursorVersion    packedHealthFieldDamage = "cursor-version"
	packedFieldEpoch            packedHealthFieldDamage = "epoch"
	packedFieldGeneration       packedHealthFieldDamage = "generation"
	packedFieldInventory        packedHealthFieldDamage = "inventory"
	packedFieldPackedRevision   packedHealthFieldDamage = "packed-revision"
	packedFieldCursorRevision   packedHealthFieldDamage = "cursor-revision"
	packedFieldCursorInventory  packedHealthFieldDamage = "cursor-inventory"
	packedFieldCursorGeneration packedHealthFieldDamage = "cursor-generation"
	packedFieldOffset           packedHealthFieldDamage = "offset"
	packedFieldPhase            packedHealthFieldDamage = "phase"
)

type packedHealthFileDamage string

const (
	packedFileChecksum       packedHealthFileDamage = "checksum"
	packedFileFence          packedHealthFileDamage = "fence"
	packedFileMissingFence   packedHealthFileDamage = "missing-fence"
	packedFileOversizeMarker packedHealthFileDamage = "oversize-marker"
	packedFileOversizeCursor packedHealthFileDamage = "oversize-cursor"
)

type packedHealthCompleteDamage string

const (
	packedCompleteMarkerVersion packedHealthCompleteDamage = "marker-version"
	packedCompleteEpoch         packedHealthCompleteDamage = "epoch"
	packedCompleteInventory     packedHealthCompleteDamage = "inventory"
)

// Small durable fixtures exercise scheduling evidence without building a packed
// inventory. Corrupt shard/registration paths must never be read by status.
func TestPackedRecoveryHealthBoundedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		complete bool
		phase    int
		want     string
	}{
		{"complete", true, 0, "complete"},
		{"shards", false, 0, "packed-shards"},
		{"fallback", false, 1, "packed-fallback"},
		{"misses", false, 2, "requested-misses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			marker, cursor := packedHealthFixture(tc.complete, tc.phase)
			writePackedHealthFixture(t, s, marker, cursor)
			for _, name := range []string{packedSessionIndexDir, "registrations", "sessions-v1"} {
				if err := os.RemoveAll(filepath.Join(s.home, name)); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(s.home, name), []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			health, err := OpenReadOnly(s.home).SessionIndexRecoveryStatus()
			if err != nil || health.Complete != tc.complete || health.Pending == tc.complete || health.Phase != tc.want {
				t.Fatalf("bounded packed evidence: %#v %v", health, err)
			}
		})
	}
}

func TestPackedRecoveryHealthRejectsInvalidEvidence(t *testing.T) {
	for _, damage := range []string{"marker-version", "cursor-version", "epoch", "generation", "inventory", "packed-revision", "cursor-revision", "cursor-inventory", "cursor-generation", "checksum", "offset", "phase", "fence", "missing-fence", "oversize-marker", "oversize-cursor"} {
		t.Run(damage, func(t *testing.T) {
			s := newTestStore(t)
			marker, cursor := packedHealthFixture(false, 0)
			switch packedHealthFieldDamage(damage) {
			case packedFieldMarkerVersion:
				marker.Version = 3
			case packedFieldCursorVersion:
				cursor.Version = 3
			case packedFieldEpoch:
				marker.PackedEpoch = "../invalid"
			case packedFieldGeneration:
				marker.Generation = ""
			case packedFieldInventory:
				marker.PackedInventory = strings.Repeat("z", 64)
			case packedFieldPackedRevision:
				marker.PackedRevision = ""
			case packedFieldCursorRevision:
				cursor.Revision = "different"
			case packedFieldCursorInventory:
				cursor.Inventory = "not a hash"
			case packedFieldCursorGeneration:
				cursor.Generation = "different"
			case packedFieldOffset:
				cursor.Offset = packedSessionIndexShards + 1
			case packedFieldPhase:
				cursor.Phase = 3
			}
			writePackedHealthFixture(t, s, marker, cursor)
			switch packedHealthFileDamage(damage) {
			case packedFileChecksum:
				cursor.Checksum = "wrong"
				writeHealthJSON(t, filepath.Join(s.home, sessionRecoveryCursorFile), cursor)
			case packedFileFence:
				writeHealthJSON(t, filepath.Join(s.home, sessionMembershipFile), sessionMembershipRevision{Version: 1, Revision: "different"})
			case packedFileMissingFence:
				if err := os.Remove(filepath.Join(s.home, sessionMembershipFile)); err != nil {
					t.Fatal(err)
				}
			case packedFileOversizeMarker:
				if err := os.WriteFile(filepath.Join(s.home, sessionIndexMarkerFile), make([]byte, 513), 0600); err != nil {
					t.Fatal(err)
				}
			case packedFileOversizeCursor:
				if err := os.WriteFile(filepath.Join(s.home, sessionRecoveryCursorFile), make([]byte, 1537), 0600); err != nil {
					t.Fatal(err)
				}
			}
			health, err := s.SessionIndexRecoveryStatus()
			if err == nil || health.Complete || health.Phase != "unknown" {
				t.Fatalf("invalid packed evidence trusted: %#v %v", health, err)
			}
		})
	}
	for _, damage := range []string{"marker-version", "epoch", "inventory", "missing-fence"} {
		t.Run("complete-"+damage, func(t *testing.T) {
			s := newTestStore(t)
			marker, cursor := packedHealthFixture(true, 0)
			switch packedHealthCompleteDamage(damage) {
			case packedCompleteMarkerVersion:
				marker.Version = 3
			case packedCompleteEpoch:
				marker.PackedEpoch = "../invalid"
			case packedCompleteInventory:
				marker.PackedInventory = ""
			}
			writePackedHealthFixture(t, s, marker, cursor)
			if damage == "missing-fence" {
				if err := os.Remove(filepath.Join(s.home, sessionMembershipFile)); err != nil {
					t.Fatal(err)
				}
			}
			health, err := s.SessionIndexRecoveryStatus()
			if err == nil || health.Complete || health.Phase != "unknown" {
				t.Fatalf("invalid completion trusted: %#v %v", health, err)
			}
		})
	}
}

func packedHealthFixture(complete bool, phase int) (sessionIndexMarker, sessionRecoveryCursor) {
	return sessionIndexMarker{Version: 2, Complete: complete, Generation: "generation", MembershipFenced: true, PackedEpoch: "epoch", PackedRevision: "revision", PackedInventory: strings.Repeat("a", 64)}, sessionRecoveryCursor{Version: 2, Generation: "generation", Revision: "revision", Inventory: strings.Repeat("b", 64), Phase: phase}
}

func writePackedHealthFixture(t *testing.T, s *Store, marker sessionIndexMarker, cursor sessionRecoveryCursor) {
	t.Helper()
	cursor.Checksum = ""
	cursor.Checksum = phaseFingerprint(cursor)
	writeHealthJSON(t, filepath.Join(s.home, sessionIndexMarkerFile), marker)
	writeHealthJSON(t, filepath.Join(s.home, sessionRecoveryCursorFile), cursor)
	writeHealthJSON(t, filepath.Join(s.home, sessionMembershipFile), sessionMembershipRevision{Version: 1, Revision: "revision"})
}

func writeHealthJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
