package state

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
)

// A same-call census is not a durable authority cache: direct corruption can
// bypass the membership revision while changing generation protection facts.
func TestPackedGenerationRereadRejectsDirectPostCensusMutation(t *testing.T) {
	for _, field := range []string{"CaptureFrozen", "PreviousGenerationID", "invalid JSON"} {
		t.Run(field, func(t *testing.T) {
			s := newTestStore(t)
			key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "generation-reread"}
			reg := migrationRegistration(key, "reread-owner")
			original, err := json.Marshal(reg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(s.registrationPath(reg.ArchiveSessionID), original, 0600); err != nil {
				t.Fatal(err)
			}
			revision, err := s.ensureSessionMembershipRevision()
			if err != nil {
				t.Fatal(err)
			}
			inventory, err := s.sessionRegistrationInventory(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(inventory[key]) != 1 {
				t.Fatal("complete census lost owner")
			}
			if _, found, err := s.loadGenerationHead(key); err != nil || found {
				t.Fatalf("head precondition: %v %v", found, err)
			}
			var changed []byte
			switch field {
			case "CaptureFrozen":
				reg.CaptureFrozen = true
			case "PreviousGenerationID":
				reg.PreviousGenerationID = "previous-owner"
			case "invalid JSON":
				changed = []byte("{")
			}
			if changed == nil {
				changed, err = json.Marshal(reg)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(s.registrationPath(reg.ArchiveSessionID), changed, 0600); err != nil {
				t.Fatal(err)
			}
			after, err := s.sessionMembershipRevision()
			if err != nil || after != revision {
				t.Fatalf("manual mutation unexpectedly fenced: %q %v", after, err)
			}
			entries := map[string]qualifiedSessionIndexEntry{}
			err = s.packedShardOwners(context.Background(), inventory, entries, time.Time{})
			if err == nil || len(entries) != 0 {
				t.Fatalf("post-census corruption authorized stale owner: %v %#v", err, entries)
			}
			if field != "invalid JSON" && !errors.Is(err, ErrSessionIndexRecoveryRequired) {
				t.Fatalf("generation mutation error: %v", err)
			}
		})
	}
}
