package state

import (
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

func TestRepeatedHookObservationDoesNotStageOrSync(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	reg := registrationFor("session")
	if err := store.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	syncs := 0
	store.onWriteSync = func() {
		syncs++
		unlock, err := local.NamedLock(store.home, requestLockName(reg.ArchiveSessionID))
		if err != nil {
			t.Fatal("observation synchronized under request lock", err)
		}
		unlock()
	}
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for _, observed := range []time.Time{at, at, at.Add(time.Minute), at.Add(time.Minute)} {
		before := syncs
		current, _, err := store.LoadRegistration(reg.ArchiveSessionID)
		if err != nil {
			t.Fatal(err)
		}
		want := 2
		if current.HookObservedAt.Equal(observed) {
			want = 0
		}
		if err := store.RecordHookObservation(reg.ArchiveSessionID, observed); err != nil {
			t.Fatal(err)
		}
		if syncs-before != want {
			t.Fatalf("observation required %d syncs, want %d", syncs-before, want)
		}
	}
}
