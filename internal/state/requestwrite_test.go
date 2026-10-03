package state

import (
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
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
	// The lock the seam tries: a new registration's is its given ID's, and
	// a subagent candidate's is its own.
	var lockName string
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
			_, err := local.RegisterNewSession(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("codex")), NativeID: "native-1"}, func(id string) archive.SessionRegistration {
				lockName = requestLockName(id)
				reg := registration(t)
				reg.ArchiveSessionID = id
				return reg
			})
			return err
		}},
		{"a subagent candidate", nil, func(local *Store) error {
			lockName = subagentLockName("child")
			return local.SaveSubagentCandidate(SubagentCandidate{ArchiveSessionID: "child", NativeSessionID: "native-1:subagent:agent", ParentArchiveSessionID: "session-1", ParentNativeSessionID: "native-1", ProjectID: "p", ProjectRoot: "/p", Harness: archive.Harness{Name: "claude"}, AgentID: "agent", TranscriptPath: "/unused", ObservedAt: at})
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
			lockName = requestLockName("session-1")
			syncs := 0
			var busy error
			local.onWriteSync = func() {
				syncs++
				unlock, err := aalocal.NamedLock(local.home, lockName)
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
				t.Fatalf("the lock was held while the write synced: %v", busy)
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
	publishing := mustRequestToken(t, collector, "session-1")
	hook := OpenReadOnly(collector.home).ForHook()
	var hookTurns []string
	syncs := 0
	collector.onWriteSync = func() {
		// The collector syncs its first staged file, which the hook then
		// overtakes; its second, which lands; and its directory, after the
		// rename, when the hook writes again.
		syncs++
		if syncs == 2 {
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
	if len(hookTurns) != 2 || syncs != 3 {
		t.Fatalf("the hook ran %d times over %d syncs, want one overtaking write and one after the rename", len(hookTurns), syncs)
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
	local.onWriteSync = func() {
		local.onWriteSync = nil
		if err := forgetter.ForgetSession(reg.ArchiveSessionID, agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(reg.Harness.Name)), NativeID: reg.NativeSessionID}); err != nil {
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

// A writer that is not a hook never syncs holding the lock: overtaken on
// every attempt, it gives up with errWriteOvertaken, for its caller to retry,
// and leaves nothing of the write behind.
func TestNonHookWriteOvertakenOnEveryAttemptGivesUp(t *testing.T) {
	at := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t)); err != nil {
		t.Fatal(err)
	}
	other := OpenReadOnly(local.home).ForHook()
	overtakes := 0
	local.onWriteSync = func() {
		overtakes++
		if err := other.SaveRequest("session-1", fmt.Sprintf("other-%d", overtakes), at); err != nil {
			t.Fatal(err)
		}
	}
	if err := local.SaveRequest("session-1", "stop", at); !errors.Is(err, errWriteOvertaken) {
		t.Fatalf("err=%v, want errWriteOvertaken", err)
	}
	if overtakes != unlockedWriteAttempts {
		t.Fatalf("overtaken %d times, want %d", overtakes, unlockedWriteAttempts)
	}
	request, _, err := local.LoadRequest("session-1")
	if err != nil || slices.Contains(request.Reasons, "stop") {
		t.Fatalf("reasons=%v err=%v: the abandoned write landed", request.Reasons, err)
	}
	assertNoWriteTemporaries(t, local)
}

// A hook, whose write cannot be retried later and must fit its budget, is
// overtaken at most once: its next attempt holds the lock, so a writer
// committing back to back cannot starve it. Nothing either writer wrote is
// lost.
func TestHookWriteOvertakenOnceLandsHoldingTheLock(t *testing.T) {
	at := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t)); err != nil {
		t.Fatal(err)
	}
	hook := local.ForHook()
	other := OpenReadOnly(local.home)
	var reasons []string
	hook.onWriteSync = func() {
		reason := fmt.Sprintf("other-%d", len(reasons))
		reasons = append(reasons, reason)
		if err := other.SaveRequest("session-1", reason, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := hook.SaveRequest("session-1", "stop", at); err != nil {
		t.Fatal(err)
	}
	// One overtaking write with the lock free, and one more after the
	// locked attempt's rename.
	if len(reasons) != hookUnlockedWriteAttempts+1 {
		t.Fatalf("the seam ran %d times, want %d", len(reasons), hookUnlockedWriteAttempts+1)
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

// A new registration does not depend on what the file held, so a change to
// the file while it is written does not make it stage and sync again.
func TestNewRegistrationIsNotRewrittenWhenTheFileChangesMidWrite(t *testing.T) {
	local := newTestStore(t)
	syncs := 0
	var id string
	local.onWriteSync = func() {
		syncs++
		if syncs == 1 {
			changed := registration(t)
			changed.ArchiveSessionID, changed.TranscriptPath = id, "/changed"
			if err := aalocal.Write(local.registrationPath(id), changed); err != nil {
				t.Fatal(err)
			}
		}
	}
	reg, err := local.RegisterNewSession(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("codex")), NativeID: "native-1"}, func(assigned string) archive.SessionRegistration {
		id = assigned
		reg := registration(t)
		reg.ArchiveSessionID = assigned
		return reg
	})
	if err != nil {
		t.Fatal(err)
	}
	if syncs != 2 {
		t.Fatalf("the registration synced %d times, want 2 (one temporary file and its directory)", syncs)
	}
	if saved, found, err := local.LoadRegistration(id); err != nil || !found || saved.TranscriptPath != reg.TranscriptPath {
		t.Fatalf("saved=%#v found=%t err=%v, want the new registration", saved, found, err)
	}
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
