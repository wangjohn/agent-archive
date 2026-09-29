package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/state"
)

// stagePhantomSubagent leaves a subagent candidate for the one session
// registered in home, as a SubagentStop hook would, observed at observedAt
// with a transcript path nothing ever writes.
func stagePhantomSubagent(t *testing.T, home string, observedAt time.Time) {
	t.Helper()
	local, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	regs, err := local.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("registrations=%+v err=%v", regs, err)
	}
	parent := regs[0]
	if err := local.SaveSubagentCandidate(state.SubagentCandidate{
		ArchiveSessionID: "phantom-child", NativeSessionID: parent.NativeSessionID + ":subagent:agent-1",
		ParentArchiveSessionID: parent.ArchiveSessionID, ParentNativeSessionID: parent.NativeSessionID,
		ProjectID: parent.ProjectID, ProjectRoot: parent.ProjectRoot, Harness: parent.Harness, AgentID: "agent-1",
		TranscriptPath: filepath.Join(t.TempDir(), "never-written.jsonl"), ObservedAt: observedAt,
	}); err != nil {
		t.Fatal(err)
	}
}

func syncAt(t *testing.T, env Env, at time.Time) (int, string, string) {
	t.Helper()
	env.Now = func() time.Time { return at }
	var out, errOut strings.Builder
	code := runSyncCommand(nil, &out, &errOut, env)
	return code, out.String(), errOut.String()
}

func statusOutput(t *testing.T, env Env, args ...string) string {
	t.Helper()
	var out, errOut strings.Builder
	runStatusCommand(args, &out, &errOut, env)
	return out.String()
}

// A subagent whose transcript is never written (Claude Code fires
// SubagentStop for some background agents with such a path) is not a sync
// failure: sync says it is waiting, then drops it once the grace period
// has passed. Default status says nothing about it; --verbose counts it.
func TestSyncPassesWithOnlyWaitingSubagents(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 21, 24, 0, 0, time.UTC)
	env, home, _, _ := publishedThroughSync(t, now)
	stagePhantomSubagent(t, home, now)

	code, out, errOut := syncAt(t, env, now.Add(time.Minute))
	if code != 0 || !strings.Contains(out, "; 1 subagent(s) waiting for transcripts.") || errOut != "" {
		t.Fatalf("sync exit=%d stdout=%q stderr=%q", code, out, errOut)
	}
	env.Now = func() time.Time { return now.Add(time.Minute) }
	if plain := statusOutput(t, env); strings.Contains(plain, "Last error") || strings.Contains(plain, "waiting for transcripts") {
		t.Fatalf("default status mentions the waiting subagent:\n%s", plain)
	}
	if verbose := statusOutput(t, env, "--verbose"); !strings.Contains(verbose, "Subagents waiting for transcripts: 1\n") || strings.Contains(verbose, "Last error") {
		t.Fatalf("verbose status:\n%s", verbose)
	}
	if asJSON := statusOutput(t, env, "--json"); !strings.Contains(asJSON, `"waiting_subagents": 1`) {
		t.Fatalf("status --json has no waiting count:\n%s", asJSON)
	}

	code, out, errOut = syncAt(t, env, now.Add(31*time.Minute))
	if code != 0 || strings.Contains(out, "subagent") || errOut != "" {
		t.Fatalf("sync after the grace exit=%d stdout=%q stderr=%q", code, out, errOut)
	}
	env.Now = func() time.Time { return now.Add(31 * time.Minute) }
	if verbose := statusOutput(t, env, "--verbose"); strings.Contains(verbose, "waiting for transcripts") || strings.Contains(verbose, "Last error") {
		t.Fatalf("verbose status after the grace:\n%s", verbose)
	}
}

// A waiting subagent does not hide a real failure in the same pass.
func TestSyncFailsOnARealErrorBesideAWaitingSubagent(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 21, 24, 0, 0, time.UTC)
	env, home, _, _ := publishedThroughSync(t, now)
	stagePhantomSubagent(t, home, now)
	// Another candidate's file no longer decodes: a real failure.
	if err := os.WriteFile(filepath.Join(home, "subagent-candidates", "unreadable.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := syncAt(t, env, now.Add(5*time.Minute))
	if code != 1 || !strings.Contains(out, "1 failed; 1 subagent(s) waiting for transcripts.") || strings.Contains(errOut, "phantom-child") {
		t.Fatalf("sync exit=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

// A status file an earlier version wrote while a waiting subagent failed
// every pass is cleared by the next pass, which counts the subagent instead.
func TestPassClearsIssuesLeftByWaitingSubagents(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 21, 24, 0, 0, time.UTC)
	env, home, _, _ := publishedThroughSync(t, now)
	stagePhantomSubagent(t, home, now)
	local, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := local.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	stale.SessionIssues = map[string]string{"phantom-child": "capture_or_publication_failed"}
	stale.SetLastErrors("1 session(s) need capture or publication")
	if err := local.SaveStatus(stale); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := syncAt(t, env, now.Add(time.Minute)); code != 0 {
		t.Fatalf("sync exit=%d stdout=%q stderr=%q", code, out, errOut)
	}
	status, err := local.LoadStatus()
	if err != nil || status.LastError != "" || len(status.LastErrors) != 0 || len(status.SessionIssues) != 0 || status.WaitingSubagents != 1 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}
