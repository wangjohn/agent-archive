package state

import (
	"reflect"
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

// A compatible writer can commit while a new admission is staged with the
// request lock free. The retry must retain that writer's immutable attribution.
func TestRegisterOrMergePreservesAnOvertakingRegistration(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	writer, err := Open(store.home)
	if err != nil {
		t.Fatal(err)
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "native-1"}
	var overtaking archive.SessionRegistration
	syncs := 0
	merged, err := store.RegisterOrMerge(key, func(id string) archive.SessionRegistration {
		proposed := registrationFor(id)
		proposed.Origin = archive.SessionOriginHook
		proposed.StartedAtSource = archive.StartedAtSourceHook
		proposed.AdmittedAt = proposed.SessionStartedAt
		proposed.DestinationID = "original-destination"
		store.onWriteSync = func() {
			syncs++
			if syncs != 1 {
				return
			}
			var err error
			overtaking, err = writer.RegisterOrMerge(key, func(assigned string) archive.SessionRegistration {
				original := registrationFor(assigned)
				original.Origin = archive.SessionOriginImport
				original.StartedAtSource = archive.StartedAtSourceTranscript
				original.SessionStartedAt = proposed.SessionStartedAt.Add(-24 * time.Hour)
				original.AdmittedAt = proposed.AdmittedAt.Add(-time.Hour)
				original.DestinationID = proposed.DestinationID
				original.TranscriptPath = "/overtaking.jsonl"
				original.ImportBatch = archive.NewImportBatch("2026-01-01-1")
				return original
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		return proposed
	})
	store.onWriteSync = nil
	if err != nil {
		t.Fatal(err)
	}
	if syncs != 3 {
		t.Fatalf("registration synced %d times, want staged retry plus directory sync", syncs)
	}
	if !reflect.DeepEqual(merged, overtaking) {
		t.Fatalf("merge replaced overtaking attribution:\n got %#v\nwant %#v", merged, overtaking)
	}
	persisted := OpenReadOnly(store.home)
	saved, found, err := persisted.LoadRegistration(merged.ArchiveSessionID)
	if err != nil || !found || !reflect.DeepEqual(saved, overtaking) {
		t.Fatalf("durable merge replaced overtaking attribution: saved=%#v found=%t err=%v", saved, found, err)
	}
	assertNoWriteTemporaries(t, store)
}
