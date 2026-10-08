package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
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
	// Replay is not an origin: --verbose keeps the capture's own.
	out.Reset()
	if code := runListCommand([]string{"--replays", "only", "--verbose", "--no-pager"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if text := out.String(); strings.Contains(text, " replay ") || !strings.Contains(text, " hook ") {
		t.Errorf("ORIGIN is not the replay's capture origin:\n%s", out.String())
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

// The terminal screen and the saved page both say when replays were counted.
func TestStatsNamesTheReplayFilter(t *testing.T) {
	t.Parallel()
	env := replayArchive(t)
	for _, tc := range []struct {
		replays string
		screen  string
		page    string
	}{
		{"include", "replays included", "replay sessions included"},
		{"only", "replays only", "replay sessions only"},
	} {
		if text := mustRunStats(t, env, 0, "--replays", tc.replays, "--no-pager"); !strings.Contains(text, tc.screen) {
			t.Errorf("--replays %s: the screen does not say %q:\n%s", tc.replays, tc.screen, text)
		}
		if page := mustRunStats(t, env, 0, "--html", "--replays", tc.replays); !strings.Contains(page, tc.page) {
			t.Errorf("--replays %s: the page does not say %q", tc.replays, tc.page)
		}
	}
	if text := mustRunStats(t, env, 0, "--no-pager"); strings.Contains(text, "replays") {
		t.Errorf("the default screen names a replay filter:\n%s", text)
	}
}

// show opens a replay by its ID, but a search of titles and names leaves it
// out, as list and the pickers do.
func TestShowFindsAReplayOnlyByItsID(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	syntheticSession{id: "replayed", harness: "claude", project: "benchmarked", captured: statsDay(time.September, 28, 10),
		turns: 1, replay: &archive.Replay{RunID: "run-7"}}.publish(t, mem)
	if _, errOut, code := runShow(t, env, "--json", "replayed"); code != 0 {
		t.Errorf("show by ID: exit %d: %s", code, errOut)
	}
	if out, errOut, code := runShow(t, env, "--json", "benchmarked"); code != 1 || !strings.Contains(errOut, "no archived session") {
		t.Errorf("a search by project found the replay: exit %d: %s\n%s", code, errOut, out)
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
	got, _ := archiveHandoffCandidates([]archive.Metadata{replay, ordinary}, map[string]bool{"p": true}, "", nil)
	if len(got) != 1 || got[0].SessionID != "mine" {
		t.Errorf("candidates = %+v, want only the ordinary session", got)
	}
	r := handoffResolver{}
	regs := []archive.SessionRegistration{
		{ArchiveSessionID: "replayed", NativeSessionID: "n1", ProjectRoot: "/work/p", Harness: archive.Harness{Name: "claude"}, Replay: &archive.Replay{RunID: "run-1"}},
	}
	if c := r.localCandidates(regs, "/work/p"); len(c.byPath) != 0 || len(c.byRepo) != 0 {
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

func TestStatusObservesReplayHooksWithoutPromotingImports(t *testing.T) {
	t.Parallel()
	home, project, userHome := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	cfg := pairTestConfig(now, []string{"codex"}, project)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	env := pairStatusEnv(t, home, userHome, now, "codex")
	env.repoKey = func(string) string { return "" }
	env.LookupEnv = func(key string) (string, bool) { return "run-status", key == archive.ReplayEnv }
	var errOut bytes.Buffer
	payload := `{"hook_event_name":"SessionStart","source":"startup","session_id":"native-replay","cwd":` + quoteJSON(project) + `}`
	if code := runHookCommand([]string{"--harness", "codex"}, strings.NewReader(payload), &errOut, env); code != 0 || errOut.Len() != 0 {
		t.Fatalf("hook code=%d err=%s", code, errOut.String())
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	imported := saveImportedSession(t, store, now, "imported-replay", project)
	imported.Replay = &archive.Replay{RunID: "run-imported"}
	if err := store.SaveRegistration(imported); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	app := view.Apps[0]
	if app.Sessions != 1 || app.ReplaySessions != 1 || app.ImportedSessions != 1 || !app.HookObserved || !app.Projects[0].HookObserved {
		t.Fatalf("replay hook/import status lost provenance: %+v", app)
	}
}

// Explicit whole and displayed short IDs can hand off a replay; title search
// and the ordinary picker still leave it out.
func TestHandoffReplayIDsRemainExplicitSelections(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"local", "archive"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			f := newPickerFixture(t)
			id := f.notUploaded
			store, err := state.Open(f.home)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.UpdateRegistration(id, func(reg *archive.SessionRegistration) error {
				reg.Replay = &archive.Replay{RunID: "explicit-replay"}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if source == "archive" {
				f.sync(t)
				f.unregister(t, id)
			} else {
				takeArchiveOffline(&f.env)
			}
			for _, query := range []string{id, shortSessionID(id)} {
				out, errOut, code := runHandoff(t, f.env, query, "--source", source)
				if code != 0 || !strings.Contains(out, "session "+id+" · source: "+source) {
					t.Errorf("explicit %q: code=%d stderr=%s\n%s", query, code, errOut, out)
				}
			}
			if out, errOut, code := runHandoff(t, f.env, "not uploaded", "--source", source); code != 1 || !strings.Contains(errOut, "no session matches") || out != "" {
				t.Errorf("title exposed replay: code=%d stderr=%s stdout=%s", code, errOut, out)
			}
			if out, errOut, code := runPicker(t, f.env, "q\n", "--source", source); code != 0 || strings.Contains(out, shortSessionID(id)) {
				t.Errorf("picker exposed replay: code=%d stderr=%s stdout=%s", code, errOut, out)
			}
		})
	}
}
