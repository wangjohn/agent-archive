package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

// unreadInput is setup's input in a test that must stop before the first
// question: any read of it is a question asked.
type unreadInput struct{ reads int }

func (u *unreadInput) Read([]byte) (int, error) {
	u.reads++
	return 0, errors.New("setup read an answer")
}

// preflightEnv is a first setup's Mac with Claude Code detected; its
// settings.json is written by each test.
func preflightEnv(t *testing.T) (env Env, home, userHome string) {
	t.Helper()
	home, userHome = t.TempDir(), t.TempDir()
	env = setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	env.DetectHarnesses = func(string) []string { return []string{"claude"} }
	env.IsTerminal = func(any) bool { return true }
	return env, home, userHome
}

// runUnanswered runs setup with args and input that fails if read, and
// returns its output and exit code.
func runUnanswered(t *testing.T, env Env, args ...string) (string, int, *unreadInput) {
	t.Helper()
	in := &unreadInput{}
	var out, errOut bytes.Buffer
	code := Run(append([]string{"setup"}, args...), in, &out, &errOut, env)
	return out.String() + errOut.String(), code, in
}

// A Claude Code settings.json with a comment, which setup cannot install
// into, stops setup before its first question, naming the file, line and
// column and the fix, and changes nothing: the file, the configuration,
// and a saved unfinished setup stay as they were.
func TestSetupStopsBeforeAnyQuestionOnACommentedSettingsFile(t *testing.T) {
	t.Parallel()
	env, home, userHome := preflightEnv(t)
	settings := filepath.Join(userHome, ".claude", "settings.json")
	must(t, os.MkdirAll(filepath.Dir(settings), 0o700))
	commented := []byte("{\n  // mine\n  \"model\": \"x\"\n}\n")
	must(t, os.WriteFile(settings, commented, 0o600))
	draft := setupDraft{Version: draftFormat, Step: 1, Config: config.Config{Harnesses: []string{"claude"}}}
	must(t, local.Write(draftPath(home), draft))
	savedDraft, err := os.ReadFile(draftPath(home))
	must(t, err)
	before := hookFileSnapshot(t, userHome)

	output, code, in := runUnanswered(t, env)
	if code != 1 || in.reads != 0 {
		t.Fatalf("exit %d after %d reads, want 1 before any question\n%s", code, in.reads, output)
	}
	for _, want := range []string{
		"✗ Claude Code hooks: ~/.claude/settings.json:2:3: ",
		"comments (JSONC) are not JSON, so remove them",
		"Fix the file",
		"✓ Background job: launchctl answers",
		"✓ Keychain: opens",
		"Nothing was changed, and any unfinished setup is kept.",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output lacks %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "unfinished setup. What would you like") || strings.Contains(output, "Step 1 of 3") {
		t.Errorf("setup went on to its questions:\n%s", output)
	}
	if after := hookFileSnapshot(t, userHome); !equalMaps(before, after) {
		t.Error("a stopped setup changed hook files")
	}
	if got, e := os.ReadFile(draftPath(home)); e != nil || !bytes.Equal(got, savedDraft) {
		t.Errorf("the saved setup changed: %s %v", got, e)
	}
	if _, found, e := config.Load(home); found || e != nil {
		t.Errorf("configuration saved: found=%v err=%v", found, e)
	}
}

// setup --yes makes the same checks before it does anything, for the apps
// it would install.
func TestSetupYesStopsOnACommentedSettingsFile(t *testing.T) {
	t.Parallel()
	env, home, userHome := preflightEnv(t)
	settings := filepath.Join(userHome, ".claude", "settings.json")
	must(t, os.MkdirAll(filepath.Dir(settings), 0o700))
	must(t, os.WriteFile(settings, []byte("{\"a\": 1,}\n"), 0o600))

	output, code, _ := runUnanswered(t, env, "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--region", "us-east-1", "--project", t.TempDir())
	if code != 1 || !strings.Contains(output, "✗ Claude Code hooks: ~/.claude/settings.json:1:8: ") {
		t.Fatalf("exit %d\n%s", code, output)
	}
	if !strings.Contains(output, "Setup incomplete: Claude Code hooks: ~/.claude/settings.json:1:8: ") || strings.Contains(output, "Checking your storage") {
		t.Errorf("setup --yes did not stop before the storage check with the problem:\n%s", output)
	}
	if strings.Contains(output, "Keychain") {
		t.Errorf("setup --yes checked the Keychain for S3:\n%s", output)
	}
	if _, found, e := config.Load(home); found || e != nil {
		t.Errorf("configuration saved: found=%v err=%v", found, e)
	}

	// Leaving Claude Code out with --apps leaves its file unchecked.
	output, _, _ = runUnanswered(t, env, "--yes", "--apps", "codex", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--region", "us-east-1", "--project", t.TempDir())
	if strings.Contains(output, "Claude Code hooks") || !strings.Contains(output, "✓ Codex hooks: ") {
		t.Errorf("setup --yes --apps codex checked other apps' files:\n%s", output)
	}
}

// A launchctl that does not say whether the job is loaded, and a job
// another installation loaded, stop setup before its first question, as
// they would stop it only when applying otherwise.
func TestSetupStopsBeforeAnyQuestionWhenLaunchctlCannotTell(t *testing.T) {
	t.Parallel()
	states := []string{"unknown", setupjournal.JobAnotherInstallation}
	wants := []string{"✗ Background job: launchctl did not say whether", "belongs to another installation"}
	for i, state := range states {
		want := wants[i]
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			env, _, _ := preflightEnv(t)
			env.JobState = func(string) string { return state }
			for _, args := range [][]string{nil, {"--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--region", "us-east-1", "--project", t.TempDir()}} {
				output, code, in := runUnanswered(t, env, args...)
				if code != 1 || in.reads != 0 || !strings.Contains(output, want) || strings.Contains(output, "Checking your storage") {
					t.Fatalf("setup %v: exit %d after %d reads\n%s", args, code, in.reads, output)
				}
			}
		})
	}
}

// A Keychain that does not open stops interactive setup before its first
// question, since R2 may be chosen there, and setup --yes when it stores in
// R2; setup --yes storing in S3 does not need it.
func TestSetupStopsBeforeAnyQuestionWhenTheKeychainDoesNotOpen(t *testing.T) {
	t.Parallel()
	env, home, _ := preflightEnv(t)
	env.Keychain = func() (credentials.CredentialStore, error) { return nil, credentials.ErrUnavailable }
	output, code, in := runUnanswered(t, env)
	if code != 1 || in.reads != 0 || !strings.Contains(output, "✗ Keychain: cannot be opened") {
		t.Fatalf("setup: exit %d after %d reads\n%s", code, in.reads, output)
	}
	output, code, _ = runUnanswered(t, env, "--yes", "--provider", "r2", "--r2-account", "0123456789abcdef0123456789abcdef", "--bucket", "b", "--r2-access-key-id", "KEY", "--project", t.TempDir())
	if code != 1 || !strings.Contains(output, "✗ Keychain: cannot be opened") {
		t.Fatalf("setup --yes --provider r2: exit %d\n%s", code, output)
	}
	if _, e := os.Stat(draftPath(home)); !os.IsNotExist(e) {
		t.Errorf("setup --yes staged a draft before stopping: %v", e)
	}
	output, code, _ = runUnanswered(t, env, "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--region", "us-east-1", "--project", t.TempDir())
	if code != 0 || strings.Contains(output, "Keychain") {
		t.Fatalf("setup --yes --provider s3: exit %d\n%s", code, output)
	}
}

// Interactive setup checks the files of the apps detected, less those the
// saved configuration leaves out, and of every app the configuration or
// the unfinished setup includes.
func TestPreflightApps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		detected []string
		saved    []string
		declined []string
		draft    []string
		want     []string
	}{
		{detected: []string{"claude", "codex"}, want: []string{"codex", "claude"}},
		{detected: []string{"claude", "cursor"}, declined: []string{"cursor"}, want: []string{"claude"}},
		{saved: []string{"cursor"}, want: []string{"cursor"}},
		{declined: []string{"codex"}, draft: []string{"codex"}, want: []string{"codex"}},
		{},
	}
	for _, c := range cases {
		if got := preflightApps(c.detected, c.saved, c.declined, c.draft); !slices.Equal(got, c.want) {
			t.Errorf("preflightApps(%v, %v, %v, %v) = %v, want %v", c.detected, c.saved, c.declined, c.draft, got, c.want)
		}
	}
}
