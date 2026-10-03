package capture

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

// This combined-stack regression keeps global admission and current-hook HEAD
// observations together without synthesizing a configured project entry.
func TestGlobalPolicyGitHeadIntegration(t *testing.T) {
	home, root, _, at := blanketHookFixture(t)
	observed := at.Add(time.Minute)
	sha := startCommit
	calls := 0
	lookup := func(dir string, dirty bool) (string, *bool) {
		calls++
		if filepath.Clean(dir) != filepath.Clean(root) {
			t.Errorf("lookup cwd = %q", dir)
		}
		if dirty {
			value := true
			return sha, &value
		}
		return sha, nil
	}
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "global-head", "cwd": root}
	if err := HandleEvent(home, "codex", payload, observed, WithDecoders(testDecoders), WithGitHead(lookup), WithReplay("global-run")); err != nil {
		t.Fatal(err)
	}
	regs := registrations(t, home)
	if len(regs) != 1 {
		t.Fatalf("registrations = %#v", regs)
	}
	reg := regs[0]
	if reg.CodexAdmission == nil || reg.Replay == nil || reg.Replay.RunID != "global-run" {
		t.Fatalf("policy/replay = %#v", reg)
	}
	if !reg.StartHead.Valid() || reg.StartHead.SHA != sha || reg.StartHead.Dirty == nil || !*reg.StartHead.Dirty || calls != 1 {
		t.Fatalf("HEAD = %#v, calls = %d", reg.StartHead, calls)
	}
	stop := map[string]any{"hook_event_name": "Stop", "session_id": "global-head", "cwd": root}
	if err := HandleEvent(home, "codex", stop, observed.Add(time.Minute), WithDecoders(testDecoders), WithGitHead(lookup), WithReplay("changed-run")); err != nil {
		t.Fatal(err)
	}
	after := registrations(t, home)[0]
	if !after.LastHead.Valid() || after.LastHead.SHA != sha || after.Replay.RunID != "global-run" || after.CodexAdmission.Generation != reg.CodexAdmission.Generation || calls != 2 {
		t.Fatalf("continuation HEAD/policy/replay = %#v, calls = %d", after, calls)
	}
}

func TestGlobalPolicyQueuedHeadReplayIntegration(t *testing.T) {
	home, root, _, at := blanketHookFixture(t)
	observed := at.Add(time.Minute)
	git := &headLookup{sha: startCommit, dirty: new(false)}
	start := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "global-queued", "cwd": root}
	batch, err := testBatch("codex", start, observed)
	if err != nil {
		t.Fatal(err)
	}
	busy := func(string, time.Duration) (func(), error) { return nil, local.ErrBusy }
	if err := handleBatch(home, "codex", batch, observed, busy, nil, eventOptions{gitHead: git.lookup, replay: archive.ParseReplay("queued-run")}); err != nil {
		t.Fatal(err)
	}
	if err := ReplayAdmissionIntents(home, observed.Add(time.Minute), testDecoders); err != nil {
		t.Fatal(err)
	}
	reg := onlyRegistration(t, home)
	if reg.CodexAdmission == nil || reg.Replay == nil || reg.Replay.RunID != "queued-run" || reg.StartHead != nil || !reg.SessionStartedAt.Equal(observed) {
		t.Fatalf("queued original proof/replay: %#v", reg)
	}
	// A queued stop retains its live HEAD observation, while replay performs no git lookup.
	stopAt := observed.Add(2 * time.Minute)
	stop := map[string]any{"hook_event_name": "Stop", "session_id": "global-queued", "cwd": root}
	batch, err = testBatch("codex", stop, stopAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := handleBatch(home, "codex", batch, stopAt, busy, nil, eventOptions{gitHead: git.lookup, replay: archive.ParseReplay("different-run")}); err != nil {
		t.Fatal(err)
	}
	if err := ReplayAdmissionIntents(home, stopAt.Add(time.Minute), testDecoders); err != nil {
		t.Fatal(err)
	}
	after := onlyRegistration(t, home)
	if after.ArchiveSessionID != reg.ArchiveSessionID || !after.LastHead.Valid() || after.LastHead.SHA != startCommit || !after.LastHead.ObservedAt.Equal(stopAt) || after.StartHead != nil || after.Replay.RunID != "queued-run" || after.CodexAdmission.Generation != reg.CodexAdmission.Generation || len(git.questions()) != 2 {
		t.Fatalf("queued stop changed ownership/proof/HEAD: %#v; questions=%#v", after, git.questions())
	}
}
