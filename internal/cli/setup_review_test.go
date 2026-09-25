package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestSetupShowsVersionsBeforeActivationAndDiscoversOnce(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now().UTC())
	calls := 0
	env.DiscoverApplications = func(string) map[string]applicationDiscovery {
		calls++
		return map[string]applicationDiscovery{"codex": {Installed: true, Version: "1.2.3", VersionState: "observed"}}
	}
	input := s3SetupInput("test", "us-east-1", "profile", true, false, false, project)
	output := setupRun(t, env, strings.TrimSuffix(input, "y\n")+"n\n", 0)
	if calls != 1 || !strings.Contains(output, "Codex 1.2.3") {
		t.Fatalf("calls %d output %s", calls, output)
	}
	if !strings.Contains(output, "Checking installed applications...") {
		t.Fatalf("discovery ran without notice: %s", output)
	}
	if _, found, _ := config.Load(home); found {
		t.Fatal("cancel activated configuration")
	}
	observations, err := readApplicationDiscoveries(home)
	if err != nil || len(observations) != 0 {
		t.Fatalf("cancel persisted discovery: %v %v", observations, err)
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestSetupReviewDoesNotCallDetectedAppsNotFound(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now().UTC())
	env.DetectHarnesses = func(string) []string { return []string{"codex"} }
	env.DiscoverApplications = func(string) map[string]applicationDiscovery {
		return map[string]applicationDiscovery{"codex": {VersionState: "absent"}}
	}
	input := strings.Join([]string{"y", project, "", "s3", "test", "profile", "us-east-1", "y"}, "\n") + "\n"
	output := setupRun(t, env, input, 0)
	if !strings.Contains(output, "Codex (version unknown)") || strings.Contains(output, "not found") {
		t.Fatalf("detected app shown as not found:\n%s", output)
	}
	// Only the display changes; the recorded discovery keeps what was seen.
	observations, err := readApplicationDiscoveries(home)
	if err != nil || observations["codex"].Installed || observations["codex"].VersionState != "absent" {
		t.Fatalf("recorded discovery changed: %+v %v", observations, err)
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestReviewDiscoveriesKeepsUndetectedAbsentApps(t *testing.T) {
	t.Parallel()
	got := reviewDiscoveries(map[string]applicationDiscovery{
		"codex":  {VersionState: "absent"},
		"claude": {VersionState: "absent"},
		"cursor": {Installed: true, Version: "3.21.13", VersionState: "observed"},
	}, []string{"claude", "cursor"})
	for app, want := range map[string]string{"codex": "Codex (not found)", "claude": "Claude Code (version unknown)", "cursor": "Cursor 3.21.13"} {
		if line := appWithVersion(app, got[app]); line != want {
			t.Fatalf("%s: got %q want %q", app, line, want)
		}
	}
}

func TestRetentionReductionShowsImpactBeforeConfirmation(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	now := time.Now().UTC()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), now)
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, project), 0)
	if err := handleHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "one", "cwd": project, "transcript_path": writeCodexTranscript(t, project)}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	env.Now = func() time.Time { return now.Add(45 * 24 * time.Hour) }
	output := setupRun(t, env, "retention\n30\nn\n", 0)
	if !strings.Contains(output, "1 session(s) captured before") {
		t.Fatal(output)
	}
	cfg, _, _ := config.Load(home)
	if cfg.RetentionDays != 90 {
		t.Fatal("decline applied shorter retention")
	}
}

// Rerunning setup from a shell with a different CLAUDE_CONFIG_DIR moves the
// hooks; the review names the files and says so before anything changes.
//
// Regression: hook ownership review, 2026-09 (1a9420b).
func TestSetupReviewWarnsWhenHookFilesMove(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	elsewhere := filepath.Join(userHome, "elsewhere")
	must(t, os.MkdirAll(elsewhere, 0700))
	env.LookupEnv = func(k string) (string, bool) { return elsewhere, k == "CLAUDE_CONFIG_DIR" }
	// Change retention (menu choice 3), keep 90 days, then cancel at review.
	var out, errOut bytes.Buffer
	Run([]string{"setup"}, strings.NewReader("3\n90\n3\n"), &out, &errOut, env)
	output := out.String()
	if !strings.Contains(output, "Claude Code hooks: "+filepath.Join(elsewhere, "settings.json")) ||
		!strings.Contains(output, "hooks move here from "+filepath.Join(userHome, ".claude", "settings.json")) {
		t.Fatalf("no warning:\n%s%s", output, &errOut)
	}
	_ = home
}

// For a configuration that never recorded its hook files, the move warning
// says an earlier release used the fixed path; it does not claim the
// variable changed since setup last ran.
//
// Regression: hook ownership second review, 2026-09 (ffee0e3).
func TestSetupReviewMoveWarningForUnrecordedHookFiles(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	cfg, _, _ := config.Load(home)
	cfg.HookFiles = nil
	must(t, config.Save(home, cfg))
	elsewhere := filepath.Join(userHome, "elsewhere")
	must(t, os.MkdirAll(elsewhere, 0700))
	env.LookupEnv = func(k string) (string, bool) { return elsewhere, k == "CLAUDE_CONFIG_DIR" }
	var out, errOut bytes.Buffer
	Run([]string{"setup"}, strings.NewReader("3\n90\n3\n"), &out, &errOut, env)
	if !strings.Contains(out.String(), "An earlier release installed them at the fixed path") || strings.Contains(out.String(), "differs from when setup last ran") {
		t.Fatalf("review:\n%s%s", &out, &errOut)
	}
}

func TestShortSetupAndReviewEdits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		edits  string
		probes int
		days   int
		prefix string
	}{
		{"accept", "y\n", 1, 90, defaultPrefix},
		{"retention", "edit\nretention\n30\ny\n", 1, 30, defaultPrefix},
		{"folder", "edit\nprefix\n../invalid\narchive/\ny\n", 2, 90, "archive/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home, project := t.TempDir(), t.TempDir()
			project, _ = filepath.EvalSymlinks(project)
			if err := os.Mkdir(filepath.Join(project, ".git"), 0700); err != nil {
				t.Fatal(err)
			}
			env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
			env.DetectHarnesses = func(string) []string { return []string{"codex", "claude"} }
			env.WorkingDir = func() (string, error) { return project, nil }
			env.AWSProfiles = func() ([]AWSProfile, error) { return []AWSProfile{{"personal", "us-west-2"}}, nil }
			probes := 0
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) { probes++; return storagetest.NewMemoryStore(), nil }
			out := setupRun(t, env, "\n\ns3\ntest-bucket\n\n"+tc.edits, 0)
			cfg, found, err := config.Load(home)
			if err != nil || !found {
				t.Fatalf("load: %v", err)
			}
			if probes != tc.probes || cfg.RetentionDays != tc.days || cfg.Storage.Prefix != tc.prefix {
				t.Fatalf("probes=%d config=%+v", probes, cfg)
			}
			if cfg.Storage.Region != "us-west-2" || cfg.Storage.AWSProfile != "personal" || len(cfg.Archive.Projects) != 1 || cfg.Archive.Projects[0].Root != project {
				t.Fatalf("unexpected config: %+v", cfg)
			}
			for _, unwanted := range []string{"Change these settings?", "Bucket region (", "Project path"} {
				if strings.Contains(out, unwanted) {
					t.Fatalf("unexpected %q: %s", unwanted, out)
				}
			}
		})
	}
}

func TestReviewEditCancellationDoesNotInstall(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		ending string
		code   int
	}{
		{"cancel", "n\n", 0}, {"EOF", "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
			input := s3SetupInput("bucket", "us-east-1", "profile", true, false, false, t.TempDir())
			input = strings.TrimSuffix(input, "y\n") + "edit\nretention\n30\n" + tc.ending
			setupRun(t, env, input, tc.code)
			if _, found, err := config.Load(home); err != nil || found {
				t.Fatalf("installed without confirmation: found=%v err=%v", found, err)
			}
		})
	}
}
