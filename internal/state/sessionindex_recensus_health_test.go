package state

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

func TestPackedRecoveryHealthConvergesAfterFreshRegistration(t *testing.T) {
	s := newTestStore(t)
	seedRecoveryInventory(t, s, 2)
	if err := s.MarkSessionIndexRecoveryNeeded(); err != nil {
		t.Fatal(err)
	}
	revision, err := s.ensureSessionMembershipRevision()
	if err != nil {
		t.Fatal(err)
	}
	// Select the real packed scheduler on a small fixture without changing its
	// production owner threshold or application allowance.
	if _, err := s.preparePackedSessionIndex(context.Background(), revision, "fixture-inventory"); err != nil {
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
		t.Fatal("initial packed recovery did not complete")
	}
	var before sessionIndexMarker
	if err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &before); err != nil {
		t.Fatal(err)
	}
	if before.Version != 2 || !before.Complete || before.PackedRevision != revision || before.PackedEpoch == "" {
		t.Fatalf("initial certificate: %#v", before)
	}
	if health, err := s.SessionIndexRecoveryStatus(); err != nil || !health.Complete || health.Pending {
		t.Fatalf("initial status: %#v %v", health, err)
	}
	key := migrationRegistrationKey(999)
	id, created, err := s.EnsureArchiveSessionID(key)
	if err != nil || !created {
		t.Fatalf("fresh identity: %q %v %v", id, created, err)
	}
	if err := s.SaveRegistration(migrationRegistration(key, id)); err != nil {
		t.Fatal(err)
	}
	nextRevision, err := s.sessionMembershipRevision()
	if err != nil || nextRevision == "" || nextRevision == revision {
		t.Fatalf("fresh membership: %q %v", nextRevision, err)
	}
	var after sessionIndexMarker
	if err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("registration changed census certificate: before=%#v after=%#v", before, after)
	}
	if health, err := OpenReadOnly(s.home).SessionIndexRecoveryStatus(); err == nil || health.Complete || health.Pending || health.Phase != "unknown" {
		t.Fatalf("uncertified membership appeared complete: %#v %v", health, err)
	}
	ids := []string{"owner-0000", "owner-0001", id}
	registrations := make(map[string][]byte, len(ids))
	for _, owner := range ids {
		data, err := os.ReadFile(s.registrationPath(owner))
		if err != nil {
			t.Fatal(err)
		}
		registrations[owner] = data
	}
	// An interrupted real scheduler pass must invalidate the old completion
	// and establish a fresh packed epoch bound to the current membership.
	if complete, err := s.RecoverSessionIndexScheduled(context.Background(), time.Nanosecond); err != nil || complete {
		t.Fatalf("stale completed census bypassed recovery: %v %v", complete, err)
	}
	if err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &after); err != nil {
		t.Fatal(err)
	}
	if after.Version != 2 || after.Complete || after.Generation == before.Generation || after.PackedEpoch == before.PackedEpoch || after.PackedEpoch == "" || after.PackedRevision != nextRevision {
		t.Fatalf("restart did not bind a new packed census: before=%#v after=%#v", before, after)
	}
	if health, err := OpenReadOnly(s.home).SessionIndexRecoveryStatus(); err != nil || !health.Pending || health.Complete || health.Phase != "packed-shards" {
		t.Fatalf("restarted census status: %#v %v", health, err)
	}
	s, err = Open(s.home)
	if err != nil {
		t.Fatal(err)
	}
	complete = false
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
		t.Fatal("fresh membership census did not converge")
	}
	if err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &after); err != nil {
		t.Fatal(err)
	}
	currentRevision, err := s.sessionMembershipRevision()
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != 2 || !after.Complete || !after.MembershipFenced || after.Generation == before.Generation || after.PackedEpoch == before.PackedEpoch || after.PackedRevision != nextRevision || after.PackedRevision != currentRevision {
		t.Fatalf("rebuilt certificate: before=%#v after=%#v current=%q", before, after, currentRevision)
	}
	for _, owner := range ids {
		data, err := os.ReadFile(s.registrationPath(owner))
		if err != nil || !bytes.Equal(data, registrations[owner]) {
			t.Fatalf("durable owner changed during rebuild: %q %v", owner, err)
		}
		reg, found, err := s.LoadRegistration(owner)
		if err != nil || !found {
			t.Fatalf("durable owner missing: %q %v %v", owner, found, err)
		}
		ownerKey, err := registrationKey(reg)
		if err != nil {
			t.Fatal(err)
		}
		got, found, err := s.ArchiveSessionID(ownerKey)
		if err != nil || !found || got != owner {
			t.Fatalf("rebuilt ownership: owner=%q got=%q found=%v err=%v", owner, got, found, err)
		}
	}
	if health, err := OpenReadOnly(s.home).SessionIndexRecoveryStatus(); err != nil || !health.Complete || health.Pending || health.Phase != "complete" {
		t.Fatalf("rebuilt status: %#v %v", health, err)
	}
}

func TestPackedCompletedSchedulerRefusesDamagedMembershipWithoutFlag(t *testing.T) {
	for _, damage := range []certificateHealthDamage{certificateDamageMissingMembership, certificateDamageCorruptMembership, certificateDamageFutureMembership, certificateDamageMembership} {
		t.Run(string(damage), func(t *testing.T) {
			s := newTestStore(t)
			marker, cursor := packedHealthFixture(true, 0)
			marker.MembershipFenced = false
			writePackedHealthFixture(t, s, marker, cursor)
			path := filepath.Join(s.home, sessionMembershipFile)
			switch damage {
			case certificateDamageMissing, certificateDamageCorrupt, certificateDamageFuture,
				certificateDamageEpoch, certificateDamageRevision, certificateDamageInventory:
				t.Fatalf("unexpected non-membership damage case: %q", damage)
			case certificateDamageMissingMembership:
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case certificateDamageCorruptMembership:
				if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case certificateDamageFutureMembership:
				writeHealthJSON(t, path, sessionMembershipRevision{Version: 2, Revision: "revision"})
			case certificateDamageMembership:
				writeHealthJSON(t, path, sessionMembershipRevision{Version: 1, Revision: "../invalid"})
			}
			if _, complete, err := s.prepareRecoveryCursor(context.Background()); err == nil || complete {
				t.Fatalf("damaged membership restarted or completed census: %v %v", complete, err)
			}
			var after sessionIndexMarker
			if err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &after); err != nil || after != marker {
				t.Fatalf("damaged membership changed marker: %#v %v", after, err)
			}
			if health, err := s.SessionIndexRecoveryStatus(); err == nil || health.Complete || health.Phase != "unknown" {
				t.Fatalf("damaged membership appeared trusted: %#v %v", health, err)
			}
		})
	}
}
