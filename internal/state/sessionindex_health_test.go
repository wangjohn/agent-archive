package state

import (
	"context"
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
