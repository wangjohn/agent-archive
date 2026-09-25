package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
)

// writeClaudeSession leaves a Claude Code transcript that ran in cwd, last
// changed at modified, where setup's project discovery finds it.
func writeClaudeSession(t *testing.T, userHome, id, cwd string, modified time.Time) {
	t.Helper()
	path := filepath.Join(userHome, ".claude", "projects", filepath.Base(cwd), id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, fmt.Appendf(nil, "{\"type\":\"user\",\"sessionId\":%q,\"cwd\":%q}\n", id, cwd), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

// gitRepo makes a resolved temporary folder that is a git repository.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Outside a repository, setup lists the projects the apps' history
// mentions, most recent first, and takes them by number; a path still works.
func TestSetupOffersProjectsFromAppHistory(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	older, newer, typed := gitRepo(t), gitRepo(t), gitRepo(t)
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	writeClaudeSession(t, userHome, "one", older, now.Add(-72*time.Hour))
	writeClaudeSession(t, userHome, "two", newer, now.Add(-time.Hour))
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)
	env.BackfillTempDirs = []string{}
	input := strings.Join([]string{"y", "n", "n", "9", "1 2", typed, "", "s3", "b", "profile", "us-east-1", "y"}, "\n") + "\n"
	output := setupRun(t, env, input, 0)
	first, second := strings.Index(output, "1) "+newer), strings.Index(output, "2) "+older)
	if first < 0 || second < first || !strings.Contains(output, "today") || !strings.Contains(output, "3 days ago") || !strings.Contains(output, "Enter numbers from 1 to 2") {
		t.Fatalf("projects not offered newest first:\n%s", output)
	}
	cfg, _, _ := config.Load(home)
	var roots []string
	for _, project := range cfg.Archive.Projects {
		roots = append(roots, project.Root)
	}
	if strings.Join(roots, ",") != strings.Join([]string{newer, older, typed}, ",") {
		t.Fatalf("projects %v", roots)
	}
}

// Accepting the current repository no longer ends the project step: one
// more question adds others, and its default keeps the old flow.
func TestSetupAddsAnotherProjectAfterTheCurrentRepository(t *testing.T) {
	t.Parallel()
	for _, add := range []bool{false, true} {
		t.Run(fmt.Sprint(add), func(t *testing.T) {
			t.Parallel()
			home, current, other := t.TempDir(), gitRepo(t), gitRepo(t)
			env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
			env.WorkingDir = func() (string, error) { return current, nil }
			answers := []string{"y", "n", "n", "y", ""}
			if add {
				answers = []string{"y", "n", "n", "y", "y", other, ""}
			}
			input := strings.Join(append(answers, "s3", "b", "profile", "us-east-1", "y"), "\n") + "\n"
			output := setupRun(t, env, input, 0)
			cfg, _, _ := config.Load(home)
			want := 1
			if add {
				want = 2
			}
			if !strings.Contains(output, "Add another project? [y/N]") || includedProjects(cfg.Archive.Projects) != want || cfg.Archive.Projects[0].Root != current {
				t.Fatalf("projects %+v\n%s", cfg.Archive.Projects, output)
			}
		})
	}
}

// Pasting the bucket URL Cloudflare shows fills in the account and the
// bucket, so the bucket is not asked for; a bad answer says what to paste.
func TestSetupTakesAccountAndBucketFromTheR2BucketURL(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	kc := newFakeKeychain()
	env := setupTestEnv(t, home, t.TempDir(), kc, time.Now())
	input := strings.Join([]string{"y", "n", "n", project, "", "r2",
		"https://0123abcd.r2.cloudflarestorage.com/my-bucket/folder",
		"https://0123abcd.r2.cloudflarestorage.com/my-bucket",
		"ACCESS", "secret-value", "y"}, "\n") + "\n"
	output := setupRun(t, env, input, 0)
	if strings.Contains(output, "Bucket name") || !strings.Contains(output, "Bucket: my-bucket") || !strings.Contains(output, "only the bucket") {
		t.Fatalf("unexpected prompts:\n%s", output)
	}
	cfg, _, _ := config.Load(home)
	if cfg.Storage.Bucket != "my-bucket" || cfg.Storage.R2AccountID != "0123abcd" || cfg.Storage.R2Endpoint != "https://0123abcd.r2.cloudflarestorage.com" {
		t.Fatalf("storage %+v", cfg.Storage)
	}
}

// Setup ends with one line per selected app on what it needs, and says that
// sessions already open are not captured.
func TestSetupNextStepsNameEachApp(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	output := setupRun(t, env, s3SetupInput("b", "us-east-1", "profile", true, true, true, t.TempDir()), 0)
	for _, want := range []string{
		"Codex: run /hooks and approve the archive hooks",
		"Claude Code: nothing to approve; start a new session.",
		"Cursor: nothing to approve; start a new Agent chat.",
		"Sessions already open are not captured",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q:\n%s", want, output)
		}
	}
}

// R2 object keys can never read public-access settings, so the review
// reminds rather than warns; status keeps reporting not_verified.
func TestSetupReviewRemindsR2UsersToCheckPublicAccess(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	output := setupRun(t, env, r2SetupInput(t.TempDir(), "secret-value"), 0)
	if strings.Contains(output, "Bucket privacy not verified") || !strings.Contains(output, "· Check that public access is disabled for the bucket in the Cloudflare dashboard.") {
		t.Fatalf("unexpected privacy note:\n%s", output)
	}
	cfg, _, _ := config.Load(home)
	if cfg.BucketPrivacy == nil || cfg.BucketPrivacy.State != "not_verified" || cfg.BucketPrivacy.Reason != r2PrivacyUnreadable {
		t.Fatalf("privacy evidence %+v", cfg.BucketPrivacy)
	}
}

// The Sessions row shows only when it is not the default, or when a
// reconfiguration changes it back to the default.
func TestSetupReviewShowsSessionsOnlyWhenNotTheDefault(t *testing.T) {
	t.Parallel()
	cfg := config.Config{Harnesses: []string{"codex"}, RetentionDays: 90}
	skills := cfg
	skills.RequireSkillUse = true
	for _, tc := range []struct {
		name          string
		next, current config.Config
		reconfiguring bool
		want          string
	}{
		{"default", cfg, config.Config{}, false, ""},
		{"skills only", skills, config.Config{}, false, "Sessions  Only new sessions that use skills"},
		{"back to all", cfg, skills, true, "* Sessions  All new sessions, with or without skills"},
	} {
		var out strings.Builder
		showSetupReview(newPrompter(strings.NewReader(""), &out), tc.next, tc.current, tc.reconfiguring, nil)
		if got := out.String(); tc.want == "" && strings.Contains(got, "Sessions") || tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("%s: review:\n%s", tc.name, got)
		}
	}
}
