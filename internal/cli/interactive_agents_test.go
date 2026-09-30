package cli

import (
	"bytes"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// A bad value must never change what a hook or the collector does: they run
// inside agents, where the switch is on by itself, and a hook must exit 0 and
// print nothing whatever the environment says.
func TestHooksAndCollectorIgnoreTheSetting(t *testing.T) {
	t.Parallel()
	payload := `{"hook_event_name":"SessionStart","source":"startup","session_id":"s-1","cwd":"/nowhere","transcript_path":"/nowhere/t.jsonl"}`
	run := func(vars map[string]string, args ...string) (code int, out, errOut string) {
		env := withEnvironment(testEnv(t, t.TempDir(), time.Now()), vars)
		env.IsTerminal = func(any) bool { return true }
		var stdout, stderr bytes.Buffer
		code = Run(args, strings.NewReader(payload), &stdout, &stderr, env)
		return code, stdout.String(), stderr.String()
	}
	for _, args := range [][]string{{"_hook", "--harness", "claude"}, {"_hook", "--harness", "codex"}, {"_hook"}, {"_collect"}} {
		wantCode, wantOut, wantErr := run(map[string]string{}, args...)
		if args[0] == "_hook" && (wantCode != 0 || wantOut != "") {
			t.Fatalf("%v: baseline code=%d stdout=%q", args, wantCode, wantOut)
		}
		for _, value := range []string{"ture", "2", "-1", "yes please", "1 0"} {
			for _, extra := range []map[string]string{{}, agentShell("CLAUDE_CODE_SESSION_ID")} {
				vars := map[string]string{envNonInteractive: value}
				maps.Copy(vars, extra)
				code, out, errOut := run(vars, args...)
				if code != wantCode || out != wantOut || errOut != wantErr || strings.Contains(errOut, envNonInteractive) {
					t.Errorf("%v with %q: code=%d stdout=%q stderr=%q; want %d %q %q", args, value, code, out, errOut, wantCode, wantOut, wantErr)
				}
			}
		}
	}
}

// Subcommand help answers before the environment is read, so a person with a
// bad setting can still look up what it should be.
func TestSubcommandHelpWorksWithABadSetting(t *testing.T) {
	t.Parallel()
	env := withEnvironment(testEnv(t, t.TempDir(), time.Now()), map[string]string{envNonInteractive: "ture"})
	for _, args := range [][]string{{"list", "--help"}, {"show", "-h"}, {"setup", "--help"}, {"help", "handoff"}, {"backfill", "undo", "--help"}} {
		var out, errOut bytes.Buffer
		if code := Run(args, nil, &out, &errOut, env); code != 0 || out.Len() == 0 || errOut.Len() != 0 {
			t.Errorf("%v: code=%d stderr=%q", args, code, &errOut)
		}
	}
}

// A terminal stdin with output going to a file is not a browsing session
// either: the picker would draw into the file and wait on a person who cannot
// see it. Both streams must be terminals.
func TestNoBrowserUnlessBothStreamsAreTerminals(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	const input = "1\nq\n"
	for _, tc := range []struct {
		name      string
		stdinTerm bool
		stdoutTty bool
		browser   bool
	}{
		{"both", true, true, true},
		{"stdout redirected", true, false, false},
		{"stdin piped", false, true, false},
		{"neither", false, false, false},
	} {
		for _, args := range [][]string{{"list"}, {"show"}} {
			stdin := strings.NewReader(input)
			var stdout, stderr bytes.Buffer
			e := env
			e.IsTerminal = func(stream any) bool {
				return (tc.stdinTerm && stream == any(stdin)) || (tc.stdoutTty && stream == any(&stdout))
			}
			code := Run(args, stdin, &stdout, &stderr, e)
			opened := strings.Contains(stdout.String(), "Enter number")
			if opened != tc.browser {
				t.Errorf("%s %v: browser opened=%v, want %v (code=%d stdout=%q)", tc.name, args, opened, tc.browser, code, stdout.String())
			}
			if !tc.browser && stdin.Len() != len(input) {
				t.Errorf("%s %v: read input without a browser", tc.name, args)
			}
			if !tc.browser && args[0] == "list" && !strings.Contains(stdout.String(), id[:minShortSessionID]) {
				t.Errorf("%s %v: no plain listing: %q", tc.name, args, stdout.String())
			}
		}
	}
}

// The guard on the R2 secret is only about a terminal that interaction is off
// for: a person at a terminal is still asked, and a script's pipe is still
// read, agent or not.
func TestSetupYesStillReadsTheR2SecretWhereItMay(t *testing.T) {
	t.Parallel()
	inAgentOverridden := agentShell("CLAUDE_CODE_SESSION_ID")
	inAgentOverridden[envNonInteractive] = "0"
	for _, tc := range []struct {
		name     string
		vars     map[string]string
		terminal bool
	}{
		{"terminal, no agent", map[string]string{}, true},
		{"terminal, agent, switch off", inAgentOverridden, true},
		{"pipe, no agent", map[string]string{}, false},
		{"pipe, agent", agentShell("CODEX_THREAD_ID"), false},
		{"pipe, explicit switch", map[string]string{envNonInteractive: "1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
			kc := newFakeKeychain()
			env := withEnvironment(setupTestEnv(t, home, userHome, kc, time.Now()), tc.vars)
			stdin := strings.NewReader("private-secret\n")
			env.IsTerminal = func(stream any) bool { return tc.terminal && stream == any(stdin) }
			var out, errOut bytes.Buffer
			code := Run([]string{"setup", "--yes", "--provider", "r2", "--r2-account", testR2Account, "--r2-access-key-id", "KEY", "--bucket", "b", "--project", project, "--apps", "codex"}, stdin, &out, &errOut, env)
			if code != 0 || stdin.Len() != 0 {
				t.Fatalf("code=%d unread=%d\n%s\n%s", code, stdin.Len(), &out, &errOut)
			}
			cfg, found, _ := config.Load(home)
			if !found {
				t.Fatal("no configuration saved")
			}
			if secret, err := kc.Load(t.Context(), cfg.Storage.R2CredentialRef); err != nil || secret.SecretAccessKey != "private-secret" {
				t.Fatalf("stored key %+v %v", secret, err)
			}
		})
	}
}

// purge apply reads a typed digest from standard input, terminal or not, so
// with interaction off it refuses instead of waiting for one.
func TestPurgeApplyDoesNotWaitForADigestInAnAgent(t *testing.T) {
	home := t.TempDir()
	setUpTestConfig(t, home, t.TempDir(), time.Now())
	env := testEnv(t, home, time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC))
	bucket := storagetest.NewMemoryStore()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return bucket, nil }
	const id = "cccccccccccccccccccccccccccccccc"
	orphan := "sessions/codex/" + id + "/source." + strings.Repeat("d", 64) + ".jsonl.gz"
	if err := bucket.Put(t.Context(), orphan, []byte("orphan")); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"purge", "plan"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("plan: code=%d stderr=%s", code, &errOut)
	}
	var planFile, digest string
	for line := range strings.SplitSeq(out.String(), "\n") {
		if value, ok := strings.CutPrefix(line, "Plan: "); ok {
			planFile = value
		}
		if value, ok := strings.CutPrefix(line, "Digest: "); ok {
			digest = value
		}
	}
	if planFile == "" || len(digest) < 12 {
		t.Fatalf("plan output: %s", &out)
	}
	answer := digest[:12] + "\n"
	if _, err := config.SetPaused(home, true); err != nil {
		t.Fatal(err)
	}
	exists := func() bool { _, err := bucket.Get(t.Context(), orphan); return err == nil }

	for _, vars := range []map[string]string{agentShell("CLAUDE_CODE_SESSION_ID"), agentShell("CODEX_THREAD_ID"), agentShell("CURSOR_AGENT"), {envNonInteractive: "1"}} {
		// Both a terminal and a pipe: nothing may wait for the digest.
		for _, isTerminal := range []bool{true, false} {
			stdin := strings.NewReader(answer)
			out.Reset()
			errOut.Reset()
			e := withEnvironment(env, vars)
			e.IsTerminal = func(any) bool { return isTerminal }
			code := Run([]string{"purge", "apply", planFile}, stdin, &out, &errOut, e)
			if code != 1 || stdin.Len() != len(answer) || out.Len() != 0 || !exists() ||
				!strings.Contains(errOut.String(), "Nothing was changed") || !strings.Contains(errOut.String(), "--yes") || !strings.Contains(errOut.String(), envNonInteractive+"=0") {
				t.Fatalf("%v terminal=%v: code=%d unread=%d stdout=%q stderr=%q", vars, isTerminal, code, stdin.Len(), &out, &errOut)
			}
		}
	}

	// Outside an agent a typed or piped digest still confirms, as before.
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"purge", "apply", planFile}, strings.NewReader("wrong\n"), &out, &errOut, env); code != 1 || !strings.Contains(errOut.String(), "Purge cancelled") || !exists() {
		t.Fatalf("wrong digest: code=%d stderr=%q", code, &errOut)
	}
	// AGENT_ARCHIVE_NONINTERACTIVE=0 gives the question back inside an agent.
	e := withEnvironment(env, map[string]string{"CLAUDE_CODE_SESSION_ID": "s", envNonInteractive: "0"})
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"purge", "apply", planFile}, strings.NewReader(answer), &out, &errOut, e); code != 0 || exists() {
		t.Fatalf("with =0: code=%d stderr=%q", code, &errOut)
	}
}
