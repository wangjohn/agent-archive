package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/local"
)

// Every help names its own command on its Usage line, uses SESSION_ID (and
// IMPORT_ID) as the only ID placeholders, and has no undocumented option
// lines: each "--flag VALUE" line has a description.
func TestHelpTextIsConsistent(t *testing.T) {
	stale := regexp.MustCompile(`\bID\b|<archive-session-id>|<id>`)
	for cmd, help := range commandHelp {
		if !strings.HasPrefix(help, "Usage: agent-archive "+cmd) {
			t.Errorf("%s: usage line %q", cmd, strings.SplitN(help, "\n", 2)[0])
		}
		if m := stale.FindString(help); m != "" {
			t.Errorf("%s: placeholder %q; use SESSION_ID or IMPORT_ID", cmd, m)
		}
		lines := strings.Split(help, "\n")
		for i, line := range lines {
			option := strings.HasPrefix(line, "  --")
			described := len(strings.Fields(strings.TrimSpace(line))) > 2 || (i+1 < len(lines) && strings.HasPrefix(lines[i+1], strings.Repeat(" ", 20)))
			if option && !described {
				t.Errorf("%s: option without a description: %q", cmd, line)
			}
		}
	}
	for _, placeholder := range []string{"agent-archive show ID", "agent-archive feedback ID"} {
		if strings.Contains(usage, placeholder) {
			t.Errorf("usage still says %q", placeholder)
		}
	}
}

// backfill undo and backfill history have help of their own, whichever way
// it is asked for.
func TestSubcommandHelp(t *testing.T) {
	for _, sub := range []string{"undo", "history"} {
		for _, args := range [][]string{{"backfill", sub, "--help"}, {"help", "backfill", sub}, {"backfill", sub, "-h"}} {
			var out bytes.Buffer
			if code := Run(args, nil, &out, nil, Env{}); code != 0 || !strings.HasPrefix(out.String(), "Usage: agent-archive backfill "+sub) {
				t.Errorf("%v: exit %d\n%s", args, code, &out)
			}
		}
	}
}

// list and backfill read --since the same way; backfill then selects whole
// local days.
func TestSinceFormsAreShared(t *testing.T) {
	loc := time.FixedZone("PDT", -7*3600)
	now := time.Date(2026, 9, 24, 1, 30, 0, 0, loc) // 08:30 UTC
	for value, want := range map[string]string{
		"":                     "",
		"2026-09-01":           "2026-09-01",
		"7d":                   "2026-09-17",
		"2h":                   "2026-09-23", // 23:30 the previous local day
		"2026-09-20T03:00:00Z": "2026-09-19", // 20:00 on the 19th, local
	} {
		got, err := backfillDay(value, now)
		if err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", value, got, err, want)
		}
	}
	for _, value := range []string{"yesterday", "-7d", "100001d", "9999999999999d"} {
		if _, err := backfillDay(value, now); err == nil {
			t.Errorf("%q accepted", value)
		}
		if _, err := parseSince(value, now); err == nil {
			t.Errorf("list accepted %q", value)
		}
	}
}

// Before setup, status says so and nothing else: no empty authentication,
// no "unknown" collector.
func TestStatusBeforeSetupIsPlain(t *testing.T) {
	env := testEnv(t, t.TempDir(), time.Now())
	var out bytes.Buffer
	if code := Run([]string{"status"}, nil, &out, nil, env); code != 0 {
		t.Fatal(code)
	}
	if want := "Agent Archive — Not set up\n\nNext: Run agent-archive setup to get started.\n"; out.String() != want {
		t.Fatalf("status:\n%q\nwant\n%q", out.String(), want)
	}
	out.Reset()
	Run([]string{"status", "--json"}, nil, &out, nil, env)
	var view map[string]any
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view["background"] != "missing" || view["authentication"].(map[string]any)["state"] != "not_configured" {
		t.Fatalf("json: background=%v authentication=%v", view["background"], view["authentication"])
	}
}

// Feedback queued while capture is paused says when it will be published.
func TestFeedbackWhilePausedSaysWhen(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	env := testEnv(t, home, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	start := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": project, "transcript_path": writeTestTranscript(t, "t.jsonl", "")}
	if err := handleHookEvent(home, "claude", start, env.now()); err != nil {
		t.Fatal(err)
	}
	regs, _ := collector.OpenLocalStoreReadOnly(home).LoadRegistrations()
	if len(regs) != 1 {
		t.Fatalf("registrations %d", len(regs))
	}
	file := filepath.Join(t.TempDir(), "feedback.txt")
	must(t, os.WriteFile(file, []byte("worked well"), 0600))
	run := func() string {
		var out, errOut bytes.Buffer
		if code := Run([]string{"feedback", regs[0].ArchiveSessionID, "--file", file}, nil, &out, &errOut, env); code != 0 {
			t.Fatalf("exit %d: %s", code, &errOut)
		}
		return out.String()
	}
	if strings.Contains(run(), "paused") {
		t.Fatal("mentions pause while not paused")
	}
	var errOut bytes.Buffer
	if code := runPauseCommand(&bytes.Buffer{}, &errOut, env, true); code != 0 {
		t.Fatal(errOut.String())
	}
	if !strings.Contains(run(), "agent-archive resume") {
		t.Fatal("paused feedback does not say when it is published")
	}
}

// Collection is called stuck only when the lock's holder took it long ago:
// a pass that started a moment ago (after the Mac woke, with the last scan
// hours old) is not, and neither is a record whose holder died, since
// nothing holds the lock then.
func TestStatusReportsAStuckCollectorLock(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	now := time.Now()
	env := pairStatusEnv(t, home, userHome, now)
	setUpTestConfig(t, home, t.TempDir(), now.Add(-48*time.Hour))
	store, _ := collector.NewLocalStore(home)
	must(t, store.SaveStatus(collector.Status{LastScanAt: now.Add(-3 * time.Hour)}))
	stuck := func() (statusView, bool) {
		t.Helper()
		view, err := readStatus(env)
		if err != nil {
			t.Fatal(err)
		}
		return view, strings.Contains(view.Next, "Collection is stuck")
	}

	unlock, err := lockCollector(home, "scheduled collection", now.Add(-5*time.Second))
	must(t, err)
	if view, isStuck := stuck(); isStuck {
		t.Fatalf("a pass that just started is stuck: %s", view.Next)
	}
	unlock()
	if _, err := os.Stat(filepath.Join(home, collectorLockRecordName)); !os.IsNotExist(err) {
		t.Fatal("releasing the lock left its record")
	}

	unlock, err = lockCollector(home, "backfill undo", now.Add(-3*time.Hour))
	must(t, err)
	view, isStuck := stuck()
	if !isStuck || view.State != "Needs attention" || !strings.Contains(view.Next, "backfill undo") || !strings.Contains(view.Next, "3h 0m ago") || !strings.Contains(view.Next, strconv.Itoa(os.Getpid())) {
		t.Fatalf("state %q next %q", view.State, view.Next)
	}
	unlock()

	// A holder that died leaves its record, but nothing holds the lock.
	must(t, local.Write(filepath.Join(home, collectorLockRecordName), collectorLockRecord{Holder: "sync", PID: 1, Since: now.Add(-3 * time.Hour)}))
	if view, isStuck := stuck(); isStuck {
		t.Fatalf("a dead holder's record reads as stuck: %s", view.Next)
	}

	// The same old record while a new holder has the lock but has not yet
	// written its own: the PID it names is gone, so status must not send the
	// user to kill it (by then the number may belong to another process).
	dead := exec.CommandContext(t.Context(), "/usr/bin/true")
	must(t, dead.Run())
	must(t, local.Write(filepath.Join(home, collectorLockRecordName), collectorLockRecord{Holder: "sync", PID: dead.Process.Pid, Since: now.Add(-3 * time.Hour)}))
	unlock, err = local.Lock(home)
	must(t, err)
	if view, isStuck := stuck(); isStuck {
		t.Fatalf("a record naming an exited process reads as stuck: %s", view.Next)
	}
	unlock()
}

// The collector's quarantined files and unrefreshable summaries, already in
// status --json, show in the text status too.
func TestStatusShowsQuarantinedFilesAndUnrefreshableSummaries(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	now := time.Now()
	env := pairStatusEnv(t, home, userHome, now)
	setUpTestConfig(t, home, t.TempDir(), now.Add(-48*time.Hour))
	store, _ := collector.NewLocalStore(home)
	must(t, store.SaveStatus(collector.Status{LastScanAt: now, QuarantinedFiles: []string{"registrations/x.json.corrupt"}, UnrefreshableSummaries: 2}))
	var out bytes.Buffer
	if code := Run([]string{"status"}, nil, &out, nil, env); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(out.String(), "Quarantined:   1 local state file(s)") || !strings.Contains(out.String(), "Summaries:     2 session summary(ies)") {
		t.Fatalf("status:\n%s", &out)
	}
}
