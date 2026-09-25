package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// setupYes runs setup with flags and no terminal, as a script would.
func setupYes(t *testing.T, env Env, stdin string, want int, args ...string) string {
	t.Helper()
	env.IsTerminal = func(any) bool { return false }
	var out, errOut bytes.Buffer
	code := Run(append([]string{"setup"}, args...), strings.NewReader(stdin), &out, &errOut, env)
	if code != want {
		t.Fatalf("setup %v: exit %d want %d\n%s\n%s", args, code, want, &out, &errOut)
	}
	return out.String() + errOut.String()
}

func withEnvironment(env Env, vars map[string]string) Env {
	env.LookupEnv = func(key string) (string, bool) { v, ok := vars[key]; return v, ok }
	return env
}

// A second Mac can be set up by a script: the R2 secret comes from an
// environment variable or standard input, never an argument, and the same
// storage check and transaction run.
func TestSetupYesConfiguresR2WithoutQuestions(t *testing.T) {
	t.Parallel()
	for _, from := range []string{"environment", "stdin"} {
		t.Run(from, func(t *testing.T) {
			t.Parallel()
			home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
			kc := newFakeKeychain()
			env := setupTestEnv(t, home, userHome, kc, time.Now())
			stdin := "private-secret\n"
			if from == "environment" {
				env = withEnvironment(env, map[string]string{envR2AccessKeyID: "KEY", envR2SecretAccessKey: "private-secret"})
				stdin = ""
			}
			args := []string{"--yes", "--provider", "r2", "--r2-account", "https://0123abcd.r2.cloudflarestorage.com/my-bucket", "--project", project, "--apps", "claude,codex"}
			if from == "stdin" {
				args = append(args, "--r2-access-key-id", "KEY")
			}
			output := setupYes(t, env, stdin, 0, args...)
			if strings.Contains(output, "private-secret") || !strings.Contains(output, "✓ Connected.") || !strings.Contains(output, "Codex: run /hooks") {
				t.Fatalf("output:\n%s", output)
			}
			cfg, found, _ := config.Load(home)
			if !found || cfg.Storage.Bucket != "my-bucket" || cfg.Storage.R2AccountID != "0123abcd" || !reflect.DeepEqual(cfg.Harnesses, []string{"codex", "claude"}) || includedProjects(cfg.Archive.Projects) != 1 {
				t.Fatalf("config %+v", cfg)
			}
			secret, err := kc.Load(context.Background(), cfg.Storage.R2CredentialRef)
			if err != nil || secret.AccessKeyID != "KEY" || secret.SecretAccessKey != "private-secret" {
				t.Fatalf("stored key %+v %v", secret, err)
			}
			if _, err := os.Stat(filepath.Join(userHome, ".codex", "hooks.json")); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(draftPath(home)); !os.IsNotExist(err) {
				t.Fatal("draft left behind")
			}
		})
	}
}

func TestSetupYesConfiguresS3WithTheProfileRegion(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	env.AWSProfiles = func() ([]AWSProfile, error) { return []AWSProfile{{"archive", "eu-west-1"}}, nil }
	env.DetectHarnesses = func(string) []string { return []string{"cursor"} }
	setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "archive", "--project", project)
	cfg, _, _ := config.Load(home)
	if cfg.Storage.Region != "eu-west-1" || cfg.Storage.AWSProfile != "archive" || !reflect.DeepEqual(cfg.Harnesses, []string{"cursor"}) {
		t.Fatalf("config %+v", cfg)
	}

	// Run again on the same Mac: another project is added, and the saved
	// storage and apps are kept.
	other := t.TempDir()
	setupYes(t, env, "", 0, "--yes", "--project", other)
	next, _, _ := config.Load(home)
	if includedProjects(next.Archive.Projects) != 2 || next.Storage != cfg.Storage || next.MachineID != cfg.MachineID {
		t.Fatalf("rerun config %+v", next)
	}

	// Switching apps declines the one left out, so setup does not offer it
	// again.
	setupYes(t, env, "", 0, "--yes", "--apps", "codex")
	next, _, _ = config.Load(home)
	if !reflect.DeepEqual(next.Harnesses, []string{"codex"}) || !reflect.DeepEqual(next.DeclinedHarnesses, []string{"cursor"}) {
		t.Fatalf("apps %v declined %v", next.Harnesses, next.DeclinedHarnesses)
	}
}

// A missing or contradictory answer is refused before anything changes.
func TestSetupYesRefusesMissingAnswers(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	for _, tc := range []struct {
		name string
		code int
		args []string
		want string
	}{
		{"no yes", 2, []string{"--provider", "s3"}, "need --yes"},
		{"no storage", 1, []string{"--yes", "--project", project, "--apps", "codex"}, "pass --provider"},
		{"no project", 1, []string{"--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--region", "r", "--apps", "codex"}, "pass --project"},
		{"no apps found", 1, []string{"--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--region", "r", "--project", project}, "pass --apps"},
		{"unknown app", 1, []string{"--yes", "--apps", "codex,vim"}, `not "vim"`},
		{"missing project", 1, []string{"--yes", "--apps", "codex", "--project", filepath.Join(project, "gone")}, "does not exist"},
		{"no region", 1, []string{"--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--project", project, "--apps", "codex"}, "pass --region"},
		{"no r2 key", 1, []string{"--yes", "--provider", "r2", "--bucket", "b", "--r2-account", "0123abcd", "--project", project, "--apps", "codex"}, envR2AccessKeyID},
		{"no r2 secret", 1, []string{"--yes", "--provider", "r2", "--bucket", "b", "--r2-account", "0123abcd", "--r2-access-key-id", "KEY", "--project", project, "--apps", "codex"}, envR2SecretAccessKey},
		{"bucket mismatch", 1, []string{"--yes", "--provider", "r2", "--bucket", "b", "--r2-account", "https://0123abcd.r2.cloudflarestorage.com/c", "--project", project, "--apps", "codex"}, "differs"},
		{"mixed providers", 1, []string{"--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--r2-account", "a", "--project", project, "--apps", "codex"}, "for --provider r2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home, userHome := t.TempDir(), t.TempDir()
			kc := newFakeKeychain()
			env := setupTestEnv(t, home, userHome, kc, time.Now())
			output := setupYes(t, env, "", tc.code, tc.args...)
			if !strings.Contains(output, tc.want) {
				t.Fatalf("missing %q:\n%s", tc.want, output)
			}
			if _, found, _ := config.Load(home); found || len(kc.items) != 0 {
				t.Fatal("a refusal changed something")
			}
			if _, err := os.Stat(filepath.Join(userHome, ".codex")); !os.IsNotExist(err) {
				t.Fatal("a refusal installed hooks")
			}
		})
	}
}

// A failed storage check leaves nothing behind: no configuration, no draft,
// and no staged key.
func TestSetupYesStorageFailureLeavesNothing(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	kc := newFakeKeychain()
	env := setupTestEnv(t, home, t.TempDir(), kc, time.Now())
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		return settingsProbeStore{storagetest.NewMemoryStore(), true}, nil
	}
	env = withEnvironment(env, map[string]string{envR2SecretAccessKey: "private-secret"})
	output := setupYes(t, env, "", 1, "--yes", "--provider", "r2", "--bucket", "b", "--r2-account", "0123abcd", "--r2-access-key-id", "KEY", "--project", project, "--apps", "codex")
	if !strings.Contains(output, "storage test failed") || strings.Contains(output, "private-secret") {
		t.Fatalf("output:\n%s", output)
	}
	if _, found, _ := config.Load(home); found || len(kc.items) != 0 {
		t.Fatal("failure left configuration or a key")
	}
	if _, err := os.Stat(draftPath(home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failure left a draft")
	}
}
