package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/hooks"
)

// awsFixture is an AWS config file outside ~/.aws, as AWS_CONFIG_FILE
// names one, whose profile "vault" gets credentials by running helper, and
// a directory (not on launchd's PATH) holding helper.
func awsFixture(t *testing.T, helper string) (configFile, credentialsFile, binDir string) {
	t.Helper()
	dir := t.TempDir()
	configFile = filepath.Join(dir, "aws config")
	credentialsFile = filepath.Join(dir, "aws credentials")
	binDir = filepath.Join(dir, "homebrew", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, helper), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := "[profile vault]\nregion = us-east-1\ncredential_process = " + helper + " exec vault --json\n"
	if err := os.WriteFile(configFile, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialsFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return configFile, credentialsFile, binDir
}

// shellEnvironment stands in for the environment setup or status runs in.
func shellEnvironment(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) { v, ok := values[name]; return v, ok }
}

func collectorPlistEnvironment(t *testing.T, env Env, home, userHome string) (map[string]string, string) {
	t.Helper()
	data, err := os.ReadFile(env.installation(home, userHome).collectorPlist())
	if err != nil {
		t.Fatal(err)
	}
	environment, err := hooks.LaunchAgentEnvironment(data)
	if err != nil {
		t.Fatal(err)
	}
	return environment, string(data)
}

// launchd starts the collector with none of the shell's environment, so
// setup writes the AWS files and PATH its storage check ran with into the
// LaunchAgent, and never the shell's AWS credentials.
//
// Regression: the plist set only AGENT_ARCHIVE_HOME, so a profile in a
// custom AWS_CONFIG_FILE, or one using credential_process from
// /opt/homebrew/bin, passed setup and never uploaded in the background.
func TestSetupGivesTheCollectorTheAWSSettingsItVerified(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	configFile, credentialsFile, binDir := awsFixture(t, "vault-helper")
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	env.LookupEnv = shellEnvironment(map[string]string{
		"AWS_CONFIG_FILE":             configFile,
		"AWS_SHARED_CREDENTIALS_FILE": credentialsFile,
		"PATH":                        binDir + ":relative/bin:/usr/bin:" + binDir,
		"AWS_ACCESS_KEY_ID":           "AKIASHELLKEYNEVERSAVED",
		"AWS_SECRET_ACCESS_KEY":       "shell-secret-never-saved",
		"AWS_SESSION_TOKEN":           "shell-token-never-saved",
	})
	output := setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "vault", true, false, false, t.TempDir()), 0)
	if strings.Contains(output, "cannot find") {
		t.Fatalf("setup warned about a helper the collector can find:\n%s", output)
	}
	environment, plist := collectorPlistEnvironment(t, env, home, userHome)
	want := map[string]string{
		"AGENT_ARCHIVE_HOME":          home,
		"AWS_CONFIG_FILE":             configFile,
		"AWS_SHARED_CREDENTIALS_FILE": credentialsFile,
		"PATH":                        binDir + ":/usr/bin:/bin:/usr/sbin:/sbin",
	}
	if len(environment) != len(want) {
		t.Fatalf("collector environment %v, want %v", environment, want)
	}
	for name, value := range want {
		if environment[name] != value {
			t.Errorf("collector %s = %q, want %q", name, environment[name], value)
		}
	}
	for _, secret := range []string{"AKIASHELLKEYNEVERSAVED", "shell-secret-never-saved", "shell-token-never-saved", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		if strings.Contains(plist, secret) {
			t.Fatalf("the LaunchAgent carries %s", secret)
		}
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Warnings) != 0 {
		t.Fatalf("status warned about the environment setup verified: %v", view.Warnings)
	}
}

// R2's credentials come from the Keychain, so its collector gets no AWS
// settings or PATH.
func TestSetupGivesAnR2CollectorOnlyItsDataDirectory(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	env.LookupEnv = shellEnvironment(map[string]string{"AWS_CONFIG_FILE": "/somewhere/config", "PATH": "/opt/homebrew/bin:/usr/bin"})
	setupRun(t, env, r2SetupInput(t.TempDir(), "r2-secret"), 0)
	environment, _ := collectorPlistEnvironment(t, env, home, userHome)
	if len(environment) != 1 || environment["AGENT_ARCHIVE_HOME"] != home {
		t.Fatalf("R2 collector environment %v", environment)
	}
}

// A credential_process setup's shell runs through an alias or function,
// not a program on PATH, is one the collector cannot run: setup says so at
// its review.
func TestSetupWarnsWhenTheCollectorCannotRunTheCredentialProcess(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	configFile, _, _ := awsFixture(t, "vault-helper")
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	env.LookupEnv = shellEnvironment(map[string]string{"AWS_CONFIG_FILE": configFile, "PATH": "/usr/bin:/bin"})
	output := setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "vault", true, false, false, t.TempDir()), 0)
	if !strings.Contains(output, `AWS profile "vault" gets its credentials by running vault-helper, which the background collector cannot find on its PATH`) {
		t.Fatalf("setup did not warn about the credential_process:\n%s", output)
	}
}

// status reads the environment the collector actually has, so it reports
// when that environment no longer loads the profile: a helper removed, an
// AWS file moved, or an older LaunchAgent that set no PATH.
func TestStatusReportsWhenTheCollectorCannotLoadTheProfile(t *testing.T) {
	t.Parallel()
	for name, drift := range map[string]func(t *testing.T, env Env, home, userHome, configFile, binDir string){
		"helper removed": func(t *testing.T, _ Env, _, _, _, binDir string) {
			t.Helper()
			if err := os.Remove(filepath.Join(binDir, "vault-helper")); err != nil {
				t.Fatal(err)
			}
		},
		"LaunchAgent without PATH": func(t *testing.T, env Env, home, userHome, configFile, _ string) {
			t.Helper()
			plistPath := env.installation(home, userHome).collectorPlist()
			executable, _ := env.executable()
			plist, err := hooks.LaunchAgent(executable, home, launchLabel(plistPath), map[string]string{"AWS_CONFIG_FILE": configFile})
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(plistPath, plist, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"config file moved": func(t *testing.T, _ Env, _, _, configFile, _ string) {
			t.Helper()
			if err := os.Rename(configFile, configFile+".old"); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home, userHome := t.TempDir(), t.TempDir()
			configFile, _, binDir := awsFixture(t, "vault-helper")
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
			env.LookupEnv = shellEnvironment(map[string]string{"AWS_CONFIG_FILE": configFile, "PATH": binDir + ":/usr/bin:/bin"})
			setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "vault", true, false, false, t.TempDir()), 0)
			drift(t, env, home, userHome, configFile, binDir)
			view, err := readStatus(env)
			if err != nil {
				t.Fatal(err)
			}
			if view.State != "Needs attention" || !strings.Contains(view.Next, "cannot load your AWS profile") {
				t.Fatalf("state %q, next %q", view.State, view.Next)
			}
			if len(view.Warnings) == 0 {
				t.Fatal("status gave no warning saying what the collector lacks")
			}
		})
	}
}

// Commands run from a shell whose AWS files differ from the collector's
// read other profiles than it does; status says so.
func TestStatusReportsAWSFilesThatDifferFromTheCollectors(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, t.TempDir()), 0)
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Warnings) != 0 {
		t.Fatalf("warnings with matching files: %v", view.Warnings)
	}
	env.LookupEnv = shellEnvironment(map[string]string{"AWS_CONFIG_FILE": "/elsewhere/config"})
	view, err = readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Warnings) != 1 || !strings.Contains(view.Warnings[0], "/elsewhere/config here, "+filepath.Join(userHome, ".aws", "config")+" for the collector") {
		t.Fatalf("warnings %v", view.Warnings)
	}
}

func TestCommandProgram(t *testing.T) {
	t.Parallel()
	for command, want := range map[string]string{
		"aws-vault exec prod --json":              "aws-vault",
		"  op\tplugin run -- aws":                 "op",
		"/opt/homebrew/bin/granted credential":    "/opt/homebrew/bin/granted",
		`"/Applications/My Tool/helper" --json`:   "/Applications/My Tool/helper",
		"~/bin/helper":                            "~/bin/helper",
		"AWS_VAULT_BACKEND=file aws-vault exec x": "",
		"$HOME/bin/helper":                        "",
		`"unterminated`:                           "",
		"":                                        "",
	} {
		if got := commandProgram(command); got != want {
			t.Errorf("commandProgram(%q) = %q, want %q", command, got, want)
		}
	}
}
