package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPackedCompletedHealthStillRequiresValidMembershipWithoutFlag(t *testing.T) {
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
			if health, err := s.SessionIndexRecoveryStatus(); err == nil || health.Complete || health.Phase != "unknown" {
				t.Fatalf("damaged unfenced completion trusted: %#v %v", health, err)
			}
		})
	}
}
