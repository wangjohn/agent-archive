package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	aalocal "github.com/wangjohn/agent-archive/internal/local"
)

func hookEvidence(turn string, at time.Time) archive.SupplementalEvidence {
	return archive.SupplementalEvidence{Kind: archive.EvidenceKindFinalResponse, ObservedAt: at, Provenance: "hook:codex:stop", Payload: map[string]any{"turn_id": turn, "text": "x"}}
}

// Every write of a file the request lock guards syncs to disk with that lock
// free: the syncs can outlast the second a hook waits for it on a busy Mac.
// The seam runs at each sync and takes the lock without waiting, as a hook
// arriving then would.
//
// Regression: the non-hook writers (the collector's notices to a parent
// session, request token migration, backfill's registrations) held the lock
// across both syncs, and a concurrent hook failed with ErrBusy, losing that
// turn's evidence.
func TestRequestLockedWritesSyncWithTheLockFree(t *testing.T) {
	at := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	// The session whose lock the seam tries: a new registration's is the ID
	// it is given.
	var lockID string
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, local *Store)
		write func(local *Store) error
	}{
		{"a collector request", func(t *testing.T, local *Store) {
			t.Helper()
			if err := local.SaveRequest("session-1", "stop", at); err != nil {
				t.Fatal(err)
			}
		}, func(local *Store) error {
			return local.SaveRequest("session-1", "subagent-published", at, hookEvidence("child", at))
		}},
		{"a request token migration", func(t *testing.T, local *Store) {
			t.Helper()
			if err := aalocal.Write(local.requestPath("session-1"), Request{ArchiveSessionID: "session-1", Reasons: []string{"stop"}, RequestedAt: at}); err != nil {
				t.Fatal(err)
			}
		}, func(local *Store) error {
			_, _, err := local.EnsureRequestToken("session-1")
			return err
		}},
		{"a registration update", nil, func(local *Store) error {
			_, err := local.UpdateRegistration("session-1", func(reg *archive.SessionRegistration) error {
				reg.TranscriptPath = "/moved"
				return nil
			})
			return err
		}},
		{"a new registration", func(t *testing.T, local *Store) {
			t.Helper()
			if err := os.Remove(local.registrationPath("session-1")); err != nil {
				t.Fatal(err)
			}
		}, func(local *Store) error {
			_, err := local.RegisterNewSession("native-1", func(id string) archive.SessionRegistration {
				lockID = id
				reg := registration(t)
				reg.ArchiveSessionID = id
				return reg
			})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := newTestStore(t)
			if err := local.SaveRegistration(registration(t)); err != nil {
				t.Fatal(err)
			}
			if tc.setup != nil {
				tc.setup(t, local)
			}
			lockID = "session-1"
			syncs := 0
			var busy error
			local.onRequestWriteSync = func() {
				syncs++
				unlock, err := aalocal.NamedLock(local.home, requestLockName(lockID))
				if err != nil {
					busy = err
					return
				}
				unlock()
			}
			if err := tc.write(local); err != nil {
				t.Fatal(err)
			}
			if busy != nil {
				t.Fatalf("the request lock was held while the write synced: %v", busy)
			}
			if syncs != 2 {
				t.Fatalf("the write synced outside the lock %d times, want 2 (its temporary file and its directory)", syncs)
			}
		})
	}
}

// A hook's request that lands while the collector writes a notice to the
// same session is written at once, and neither write loses the other's
// evidence: the collector's write, overtaken, is merged again onto the
// hook's request. The token the collector was publishing no longer covers
// the request, so acknowledging it leaves the hook's evidence pending.
func TestHookRequestLandingDuringACollectorWriteIsKept(t *testing.T) {
	at := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	collector := newTestStore(t)
	if err := collector.SaveRegistration(registration(t)); err != nil {
		t.Fatal(err)
	}
	if err := collector.SaveRequest("session-1", "stop", at, hookEvidence("published", at)); err != nil {
		t.Fatal(err)
	}
	publishing := mustRequestTokenOf(t, collector, "session-1")
	hook := OpenReadOnly(collector.home)
	var hookTurns []string
	collector.onRequestWriteSync = func() {
		// At both of the collector's syncs, before and after its rename.
		if len(hookTurns) >= 4 {
			return
		}
		turn := fmt.Sprintf("turn-%d", len(hookTurns))
		hookTurns = append(hookTurns, turn)
		if err := hook.SaveRequest("session-1", "stop", at, hookEvidence(turn, at)); err != nil {
			t.Fatalf("the hook's request failed while the collector wrote: %v", err)
		}
	}
	if err := collector.SaveRequest("session-1", "subagent-published", at, hookEvidence("child", at)); err != nil {
		t.Fatal(err)
	}
	if len(hookTurns) < 2 {
		t.Fatalf("the hook ran %d times, want at least one overtaking write and one after the rename", len(hookTurns))
	}
	request, found, err := collector.LoadRequest("session-1")
	if err != nil || !found {
		t.Fatalf("found=%t err=%v", found, err)
	}
	for _, turn := range append([]string{"published", "child"}, hookTurns...) {
		if !requestHasEvidence(request.HookEvidence, hookEvidence(turn, at)) {
			t.Fatalf("evidence %q was lost: %#v", turn, request.HookEvidence)
		}
	}
	if !slices.Contains(request.Reasons, "subagent-published") {
		t.Fatalf("reasons=%v, want the collector's", request.Reasons)
	}
	if completed, err := collector.CompleteRequest("session-1", publishing); err != nil || completed {
		t.Fatalf("completed=%t err=%v: the published token acknowledged evidence it did not cover", completed, err)
	}
	assertNoWriteTemporaries(t, collector)
}

// A session forgotten while a request for it is being written refuses the
// request under the lock, as before: nothing is left of the write.
func TestRequestForASessionForgottenMidWriteIsRefused(t *testing.T) {
	at := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	local := newTestStore(t)
	reg := registration(t)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	forgetter := OpenReadOnly(local.home)
	local.onRequestWriteSync = func() {
		local.onRequestWriteSync = nil
		if err := forgetter.ForgetSession(reg.ArchiveSessionID, reg.NativeSessionID); err != nil {
			t.Fatal(err)
		}
	}
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", at, hookEvidence("late", at)); !errors.Is(err, ErrSessionNotRegistered) {
		t.Fatalf("err=%v, want ErrSessionNotRegistered", err)
	}
	if _, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || found {
		t.Fatalf("an orphan request was written: found=%t err=%v", found, err)
	}
	if _, err := os.Stat(filepath.Join(local.home, requestLockName(reg.ArchiveSessionID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused request left its lock file: %v", err)
	}
	assertNoWriteTemporaries(t, local)
}

// A write that another writer overtakes on every attempt with the lock free
// makes its last attempt holding the lock, so it lands: a writer committing
// back to back cannot starve it. Nothing either writer wrote is lost.
func TestRequestWriteOvertakenOnEveryAttemptLandsHoldingTheLock(t *testing.T) {
	at := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t)); err != nil {
		t.Fatal(err)
	}
	other := OpenReadOnly(local.home)
	var reasons []string
	local.onRequestWriteSync = func() {
		reason := fmt.Sprintf("other-%d", len(reasons))
		reasons = append(reasons, reason)
		if err := other.SaveRequest("session-1", reason, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := local.SaveRequest("session-1", "stop", at); err != nil {
		t.Fatal(err)
	}
	// One overtaking write per attempt with the lock free, and one more
	// after the locked attempt's rename.
	if len(reasons) != requestWriteAttempts {
		t.Fatalf("the seam ran %d times, want %d", len(reasons), requestWriteAttempts)
	}
	request, _, err := local.LoadRequest("session-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range append([]string{"stop"}, reasons...) {
		if !slices.Contains(request.Reasons, reason) {
			t.Fatalf("reasons=%v, want %q", request.Reasons, reason)
		}
	}
	assertNoWriteTemporaries(t, local)
}

func mustRequestTokenOf(t *testing.T, local *Store, id string) string {
	t.Helper()
	request, found, err := local.LoadRequest(id)
	if err != nil || !found || request.Token == "" {
		t.Fatalf("request=%#v found=%t err=%v", request, found, err)
	}
	return request.Token
}

func assertNoWriteTemporaries(t *testing.T, local *Store) {
	t.Helper()
	for _, dir := range []string{"requests", "registrations"} {
		entries, err := os.ReadDir(filepath.Join(local.home, dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".pending-") {
				t.Fatalf("a temporary file was left behind: %s/%s", dir, entry.Name())
			}
		}
	}
}
