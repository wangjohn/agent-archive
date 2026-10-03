package state

import (
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestRepeatedAdmissionPreservesOriginalProvenance(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "native-1"}
	original, err := store.RegisterOrMerge(key, func(id string) archive.SessionRegistration {
		reg := registrationFor(id)
		reg.Origin = archive.SessionOriginImport
		reg.AdmittedAt = reg.SessionStartedAt.Add(time.Hour)
		reg.DestinationID = "original-destination"
		return reg
	})
	if err != nil {
		t.Fatal(err)
	}
	merged, err := store.RegisterOrMerge(key, func(id string) archive.SessionRegistration {
		reg := registrationFor(id)
		reg.Origin = archive.SessionOriginHook
		reg.SessionStartedAt = original.SessionStartedAt.Add(24 * time.Hour)
		reg.AdmittedAt = reg.SessionStartedAt
		reg.DestinationID = original.DestinationID
		reg.TranscriptPath = "/different.jsonl"
		return reg
	})
	if err != nil {
		t.Fatal(err)
	}
	if merged.ArchiveSessionID != original.ArchiveSessionID || merged.Origin != original.Origin || !merged.AdmittedAt.Equal(original.AdmittedAt) || !merged.SessionStartedAt.Equal(original.SessionStartedAt) || merged.TranscriptPath != original.TranscriptPath {
		t.Fatalf("repeated admission replaced original attribution: %#v", merged)
	}
}
