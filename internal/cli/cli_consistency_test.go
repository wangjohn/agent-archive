package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
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
	for _, value := range []string{"yesterday", "-7d"} {
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
	if view["background"] != "not_installed" || view["authentication"].(map[string]any)["state"] != "not_configured" {
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
	os.WriteFile(file, []byte("worked well"), 0600)
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

// A collector lock held long past a pass's time limit, with no scan
// finishing, is reported as stuck rather than as "run sync", which could
// not get the lock either.
func TestStatusReportsAStuckCollectorLock(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	now := time.Now()
	env := pairStatusEnv(t, home, userHome, now)
	setUpTestConfig(t, home, t.TempDir(), now.Add(-48*time.Hour))
	store, _ := collector.NewLocalStore(home)
	if err := store.SaveStatus(collector.Status{LastScanAt: now.Add(-45 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(view.Next, "collector lock") {
		t.Fatalf("stuck with the lock free: %s", view.Next)
	}
	unlock, err := local.Lock(home)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	view, err = readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.State != "Needs attention" || !strings.Contains(view.Next, "collector lock") || !strings.Contains(view.Next, "45m") {
		t.Fatalf("state %q next %q", view.State, view.Next)
	}
}
