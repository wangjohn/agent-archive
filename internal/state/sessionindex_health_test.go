package state

import (
	"context"
	"encoding/json"
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
			switch damage {
			case "marker-version":
				marker.Version = 3
			case "cursor-version":
				cursor.Version = 3
			case "epoch":
				marker.PackedEpoch = "../invalid"
			case "generation":
				marker.Generation = ""
			case "inventory":
				marker.PackedInventory = strings.Repeat("z", 64)
			case "packed-revision":
				marker.PackedRevision = ""
			case "cursor-revision":
				cursor.Revision = "different"
			case "cursor-inventory":
				cursor.Inventory = "not a hash"
			case "cursor-generation":
				cursor.Generation = "different"
			case "offset":
				cursor.Offset = packedSessionIndexShards + 1
			case "phase":
				cursor.Phase = 3
			}
			writePackedHealthFixture(t, s, marker, cursor)
			switch damage {
			case "checksum":
				cursor.Checksum = "wrong"
				writeHealthJSON(t, filepath.Join(s.home, sessionRecoveryCursorFile), cursor)
			case "fence":
				writeHealthJSON(t, filepath.Join(s.home, sessionMembershipFile), sessionMembershipRevision{Version: 1, Revision: "different"})
			case "missing-fence":
				if err := os.Remove(filepath.Join(s.home, sessionMembershipFile)); err != nil {
					t.Fatal(err)
				}
			case "oversize-marker":
				if err := os.WriteFile(filepath.Join(s.home, sessionIndexMarkerFile), make([]byte, 513), 0600); err != nil {
					t.Fatal(err)
				}
			case "oversize-cursor":
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
			switch damage {
			case "marker-version":
				marker.Version = 3
			case "epoch":
				marker.PackedEpoch = "../invalid"
			case "inventory":
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
