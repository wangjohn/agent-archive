package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

// replayArchive publishes one ordinary session and one replay.
func replayArchive(t *testing.T) Env {
	t.Helper()
	env, mem := statsEnv(t)
	tokens := []modelTokenSpec{{"claude-opus-5", 1000, 1000, 0, 0}}
	syntheticSession{id: "mine", harness: "claude", project: "p", captured: statsDay(time.September, 28, 9),
		models: []string{"claude-opus-5"}, turns: 1, perModel: tokens}.publish(t, mem)
	syntheticSession{id: "replayed", harness: "claude", project: "p", captured: statsDay(time.September, 28, 10),
		models: []string{"claude-opus-5"}, turns: 1, perModel: tokens, replay: &archive.Replay{RunID: "run-7"}}.publish(t, mem)
	return env
}

func listedIDs(t *testing.T, env Env, args ...string) []string {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := runListCommand(append([]string{"--json"}, args...), strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatalf("list %v: exit %d: %s", args, code, errOut.String())
	}
	var doc struct {
		Sessions []archive.Metadata `json:"sessions"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range doc.Sessions {
		ids = append(ids, m.SessionID)
	}
	return ids
}

// list hides replays unless asked, with and without the index's limit, and
// a --json consumer that includes them can tell them apart by replay.
func TestListHidesReplaysUnlessAsked(t *testing.T) {
	t.Parallel()
	env := replayArchive(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "mine"},
		{[]string{"--limit", "0"}, "mine"},
		{[]string{"--limit", "1"}, "mine"},
		{[]string{"--replays", "hide"}, "mine"},
		{[]string{"--replays", "include"}, "replayed mine"},
		{[]string{"--replays", "only"}, "replayed"},
		{[]string{"--replays", "only", "--hook-captured"}, "replayed"},
	} {
		if got := strings.Join(listedIDs(t, env, tc.args...), " "); got != tc.want {
			t.Errorf("list %v = %q, want %q", tc.args, got, tc.want)
		}
	}
	var out, errOut bytes.Buffer
	if code := runListCommand([]string{"--replays", "all"}, strings.NewReader(""), &out, &errOut, env); code != 2 || !strings.Contains(errOut.String(), "--replays must be hide, include, or only") {
		t.Errorf("--replays all: exit %d, stderr %q", code, errOut.String())
	}
}

// A replay shown with --replays include is marked in the table.
func TestListMarksAReplayInTheTable(t *testing.T) {
	t.Parallel()
	env := replayArchive(t)
	var out, errOut bytes.Buffer
	if code := runListCommand([]string{"--replays", "include", "--no-pager"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "[replay]") {
		t.Errorf("no replay mark:\n%s", out.String())
	}
}

func TestStatsLeavesOutReplaysUnlessAsked(t *testing.T) {
	t.Parallel()
	env := replayArchive(t)
	for args, want := range map[string]int{"": 1, "include": 2, "only": 1} {
		a := []string{"--json"}
		if args != "" {
			a = append(a, "--replays", args)
		}
		var doc statsDocument
		if err := json.Unmarshal([]byte(mustRunStats(t, env, 0, a...)), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Coverage.Sessions != want || doc.Filters.Replays != args {
			t.Errorf("%v: %d sessions, filters %+v; want %d", a, doc.Coverage.Sessions, doc.Filters, want)
		}
	}
}

// The hook command reads AGENT_ARCHIVE_REPLAY from its environment.
func TestHookCommandMarksASessionStartedWithTheReplayVariable(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, now.Add(-time.Hour))
	env := testEnv(t, home, now)
	env.repoKey = func(string) string { return "" }
	env.LookupEnv = func(key string) (string, bool) {
		if key == archive.ReplayEnv {
			return "bench-run-3", true
		}
		return "", false
	}
	payload := `{"hook_event_name":"SessionStart","source":"startup","session_id":"native-1","cwd":` + quoteJSON(project) + `}`
	var errOut bytes.Buffer
	if code := runHookCommand([]string{"--harness", "codex"}, strings.NewReader(payload), &errOut, env); code != 0 || errOut.Len() > 0 {
		t.Fatalf("code %d, stderr %q", code, errOut.String())
	}
	regs, err := state.OpenReadOnly(home).LoadRegistrations()
	if err != nil || len(regs) != 1 || regs[0].Replay == nil || regs[0].Replay.RunID != "bench-run-3" {
		t.Fatalf("registrations = %#v, err = %v", regs, err)
	}
}

// A replay never becomes the latest session to hand off.
func TestHandoffLatestPassesOverReplays(t *testing.T) {
	t.Parallel()
	ordinary := archive.Metadata{SessionID: "mine", ProjectID: "p"}
	replay := archive.Metadata{SessionID: "replayed", ProjectID: "p", Replay: &archive.Replay{}}
	got := archiveHandoffCandidates([]archive.Metadata{replay, ordinary}, map[string]bool{"p": true}, nil)
	if len(got) != 1 || got[0].SessionID != "mine" {
		t.Errorf("candidates = %+v, want only the ordinary session", got)
	}
	r := handoffResolver{}
	regs := []archive.SessionRegistration{
		{ArchiveSessionID: "replayed", NativeSessionID: "n1", ProjectRoot: "/work/p", Harness: archive.Harness{Name: "claude"}, Replay: &archive.Replay{RunID: "run-1"}},
	}
	if c := r.localCandidates(regs, "/work/p"); len(c) != 0 {
		t.Errorf("local candidates = %+v, want the replay passed over", c)
	}
}

// Replays are hook captures, so status counts them as sessions, and says how
// many of them are replays.
func TestStatusCountsReplaysAmongAnAppsSessions(t *testing.T) {
	t.Parallel()
	if got := appCounts(appStatus{Sessions: 3, ReplaySessions: 2}); got != "3 sessions, 2 of them replays" {
		t.Errorf("appCounts = %q", got)
	}
	if got := appCounts(appStatus{Sessions: 3}); got != "3 sessions" {
		t.Errorf("appCounts without replays = %q", got)
	}
}
