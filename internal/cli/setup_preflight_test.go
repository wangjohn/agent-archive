package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
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
		"✗ Claude Code hooks: ~/.claude/settings.json:2:3\n",
		"comments (JSONC) are not JSON, so remove them",
		"Fix the file, then run",
		"✓ Background job: launchctl responds",
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
	if code != 1 || !strings.Contains(output, "✗ Claude Code hooks: ~/.claude/settings.json:1:8\n") {
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

// A Keychain that does not open stops setup before its first question when
// it would store in R2: interactive setup whose saved or unfinished setup
// stores there, and setup --yes --provider r2. A setup that may still
// choose S3 goes on to its questions, and setup --yes storing in S3 does
// not need the Keychain.
func TestSetupStopsBeforeAnyQuestionWhenTheKeychainDoesNotOpen(t *testing.T) {
	t.Parallel()
	env, home, _ := preflightEnv(t)
	env.Keychain = func() (credentials.CredentialStore, error) { return nil, credentials.ErrUnavailable }
	output, _, in := runUnanswered(t, env)
	if in.reads == 0 || strings.Contains(output, "Keychain") {
		t.Fatalf("a first setup did not reach its first question without checking the Keychain: %d reads\n%s", in.reads, output)
	}
	output, code, _ := runUnanswered(t, env, "--yes", "--provider", "r2", "--r2-account", "0123456789abcdef0123456789abcdef", "--bucket", "b", "--r2-access-key-id", "KEY", "--project", t.TempDir())
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

	// An unfinished setup that stores in R2 stops before its first question.
	draft := setupDraft{Version: draftFormat, Step: 2, Config: config.Config{Harnesses: []string{"claude"}, Storage: credentials.Config{Provider: credentials.ProviderR2, Bucket: "b"}}}
	must(t, local.Write(draftPath(home), draft))
	output, code, in = runUnanswered(t, env)
	if code != 1 || in.reads != 0 || !strings.Contains(output, "✗ Keychain: cannot be opened") {
		t.Fatalf("setup with an R2 draft: exit %d after %d reads\n%s", code, in.reads, output)
	}
}

// A configuration saved with R2 makes interactive setup check the Keychain
// before its first question.
func TestSetupChecksTheKeychainForASavedR2Configuration(t *testing.T) {
	t.Parallel()
	env, home, _ := preflightEnv(t)
	cfg := config.Config{Harnesses: []string{"claude"}, Storage: credentials.Config{Provider: credentials.ProviderR2, Bucket: "b"}}
	must(t, config.Save(home, cfg))
	output, _, _ := runUnanswered(t, env)
	if !strings.Contains(output, "✓ Keychain: opens") {
		t.Fatalf("setup did not check the Keychain for a saved R2 configuration:\n%s", output)
	}
	env.Keychain = func() (credentials.CredentialStore, error) { return nil, credentials.ErrUnavailable }
	output, code, in := runUnanswered(t, env)
	if code != 1 || in.reads != 0 || !strings.Contains(output, "✗ Keychain: cannot be opened") {
		t.Fatalf("exit %d after %d reads\n%s", code, in.reads, output)
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

// An app the unfinished setup leaves out is not checked: resuming it never
// touches that app's file, so a file setup cannot edit does not stop it.
func TestSetupDoesNotCheckAnAppTheUnfinishedSetupLeavesOut(t *testing.T) {
	t.Parallel()
	env, home, userHome := preflightEnv(t)
	env.DetectHarnesses = func(string) []string { return []string{"claude", "cursor"} }
	cursor := env.hookFiles(userHome)["cursor"]
	must(t, os.MkdirAll(filepath.Dir(cursor), 0o700))
	must(t, os.WriteFile(cursor, []byte("{,}\n"), 0o600))
	draft := setupDraft{Version: draftFormat, Step: 1, Config: config.Config{Harnesses: []string{"claude"}, DeclinedHarnesses: []string{"cursor"}}}
	must(t, local.Write(draftPath(home), draft))

	output, _, in := runUnanswered(t, env)
	if in.reads == 0 || strings.Contains(output, "Cursor hooks") || !strings.Contains(output, "unfinished setup. What would you like") {
		t.Fatalf("setup did not reach the unfinished setup's menu without checking Cursor: %d reads\n%s", in.reads, output)
	}
}

// The fix for a hook file offers leaving the app out with setup --yes
// --apps only where that runs: not for an installed app, which --yes
// never removes, and not while an unfinished setup is saved, which --yes
// refuses. setup --yes, stopped, says to run the same command again.
func TestPreflightFixOffersOnlyARunnableSetupYes(t *testing.T) {
	t.Parallel()
	commented := []byte("{\n  // mine\n}\n")
	leaveOut := "To set up without Claude Code, run agent-archive setup --yes"

	env, home, userHome := preflightEnv(t)
	settings := env.hookFiles(userHome)["claude"]
	must(t, os.MkdirAll(filepath.Dir(settings), 0o700))
	must(t, os.WriteFile(settings, commented, 0o600))
	cfg := config.Config{
		Harnesses: []string{"claude"},
		Storage:   credentials.Config{Provider: credentials.ProviderS3, Bucket: "b", AWSProfile: "p", Region: "us-east-1"},
		Archive:   archive.Config{Enabled: true},
	}
	must(t, config.Save(home, cfg))
	output, code, _ := runUnanswered(t, env)
	if code != 1 || !strings.Contains(output, "✗ Claude Code hooks") || strings.Contains(output, leaveOut) {
		t.Errorf("installed: exit %d\n%s", code, output)
	}
	output, code, _ = runUnanswered(t, env, "--yes", "--project", t.TempDir())
	if code != 1 || strings.Contains(output, leaveOut) || !strings.Contains(output, "run the same agent-archive setup --yes command again") || strings.Contains(output, "unfinished setup is kept") {
		t.Errorf("installed, --yes: exit %d\n%s", code, output)
	}

	env, home, userHome = preflightEnv(t)
	settings = env.hookFiles(userHome)["claude"]
	must(t, os.MkdirAll(filepath.Dir(settings), 0o700))
	must(t, os.WriteFile(settings, commented, 0o600))
	output, _, _ = runUnanswered(t, env)
	if !strings.Contains(output, leaveOut) {
		t.Errorf("first setup: the fix does not offer --apps\n%s", output)
	}
	must(t, local.Write(draftPath(home), setupDraft{Version: draftFormat, Step: 1, Config: config.Config{Harnesses: []string{"claude"}}}))
	output, code, _ = runUnanswered(t, env)
	if code != 1 || strings.Contains(output, leaveOut) {
		t.Errorf("unfinished setup saved: exit %d\n%s", code, output)
	}
}

// lockedKeychain is a Keychain this build can use but that refuses reads
// without UI, as a locked login Keychain does.
type lockedKeychain struct{ *fakeKeychain }

func (lockedKeychain) Load(context.Context, string) (credentials.R2Credentials, error) {
	return credentials.R2Credentials{}, credentials.ErrKeychainLocked
}

// A Keychain that opens in this build but is locked stops setup before its
// first question too, with the fix of unlocking it; a saved key that is
// merely missing does not.
func TestSetupStopsBeforeAnyQuestionWhenTheKeychainIsLocked(t *testing.T) {
	t.Parallel()
	env, home, _ := preflightEnv(t)
	must(t, config.Save(home, config.Config{Harnesses: []string{"claude"}, Storage: credentials.Config{Provider: credentials.ProviderR2, Bucket: "b", R2CredentialRef: "saved"}}))
	env.Keychain = func() (credentials.CredentialStore, error) { return lockedKeychain{newFakeKeychain()}, nil }
	output, code, in := runUnanswered(t, env)
	if code != 1 || in.reads != 0 || !strings.Contains(output, "✗ Keychain: cannot be opened") || !strings.Contains(output, "Unlock the login Keychain") {
		t.Fatalf("locked: exit %d after %d reads\n%s", code, in.reads, output)
	}
	env.Keychain = func() (credentials.CredentialStore, error) { return newFakeKeychain(), nil }
	if output, _, _ = runUnanswered(t, env); !strings.Contains(output, "✓ Keychain: opens") {
		t.Fatalf("a missing saved key failed the Keychain check:\n%s", output)
	}
}

// setup --yes reports a mistake in its flags before asking launchctl or the
// Keychain, so one run names it even when those would stop setup too.
func TestSetupYesReportsFlagMistakesBeforeItsChecks(t *testing.T) {
	t.Parallel()
	env, _, _ := preflightEnv(t)
	env.JobState = func(string) string { return "unknown" }
	output, code, _ := runUnanswered(t, env, "--yes", "--provider", "r2", "--bucket", "b", "--project", t.TempDir())
	if code != 1 || !strings.Contains(output, "--provider r2 needs --r2-account") || strings.Contains(output, "Background job") {
		t.Fatalf("exit %d\n%s", code, output)
	}
}

// A hook file's reason becomes one sentence, whatever its ending.
func TestSentence(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"": "", "remove them": "Remove them.", "remove them.": "Remove them.", "é": "É."} {
		if got := sentence(in); got != want {
			t.Errorf("sentence(%q) = %q, want %q", in, got, want)
		}
	}
}
