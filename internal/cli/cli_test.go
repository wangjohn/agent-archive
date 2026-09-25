package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// noEnv is an empty process environment.
func noEnv(string) (string, bool) { return "", false }

func testEnv(t *testing.T, home string, now time.Time) Env {
	t.Helper()
	userHome, accountHome := t.TempDir(), t.TempDir()
	return Env{
		UserHomeDir: func() (string, error) { return userHome, nil },
		// Tests never run as the account's default installation.
		AccountHome: func() (string, error) { return accountHome, nil },
		Home:        func() (string, error) { return home, nil },
		Now:         func() time.Time { return now },
		// Tests must not see the real environment: run inside an agent,
		// CLAUDE_CODE_SESSION_ID would change what `handoff --latest` skips.
		LookupEnv: func(string) (string, bool) { return "", false },
		OpenStore: func(config.Config) (storage.ObjectStore, error) {
			return storage.NewMemoryStore(), nil
		},
		// Everything below would otherwise reach this Mac itself. Reads get
		// a harmless answer; anything that would change the Mac fails the
		// test. A test that needs one sets it (setupTestEnv sets them all).
		JobState: func(string) string { return "missing" },
		LoadLaunchAgent: func(plist string) error {
			t.Errorf("unexpected LaunchAgent load of %s: set Env.LoadLaunchAgent", plist)
			return errors.New("no launchd in this test")
		},
		UnloadLaunchAgent: func(plist string) error {
			t.Errorf("unexpected LaunchAgent unload of %s: set Env.UnloadLaunchAgent", plist)
			return errors.New("no launchd in this test")
		},
		Keychain: func() (credentials.CredentialStore, error) {
			return nil, errors.New("no Keychain in this test: set Env.Keychain")
		},
		Executable: func() (string, error) {
			return "", errors.New("no executable in this test: set Env.Executable")
		},
		WorkingDir:           func() (string, error) { return "", errors.New("no working directory in this test") },
		AWSProfiles:          func() ([]AWSProfile, error) { return nil, nil },
		DetectHarnesses:      func(string) []string { return nil },
		DiscoverApplications: func(string) map[string]applicationDiscovery { return map[string]applicationDiscovery{} },
	}
}

// credentialsTestConfig is a syntactically valid storage destination for
// tests that never actually touch storage (they use OpenStore above, or
// test hook logic that writes only local files).
func credentialsTestConfig() credentials.Config {
	return credentials.Config{Provider: credentials.ProviderS3, Bucket: "test-bucket", Region: "us-east-1", AWSProfile: "test", Prefix: "agent-archive/"}
}

func TestHelpAndVersion(t *testing.T) {
	var out bytes.Buffer
	if code := Run([]string{"--help"}, nil, &out, nil, Env{}); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out.String(), "agent-archive setup") {
		t.Fatalf("help output missing usage: %s", out.String())
	}

	out.Reset()
	if code := Run([]string{"--version"}, nil, &out, nil, Env{}); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if strings.TrimSpace(out.String()) != Version {
		t.Fatalf("version output=%q want=%q", out.String(), Version)
	}
}

func TestUnknownCommandAndNoArgs(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"bogus"}, nil, &out, &errOut, Env{}); code != 2 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(errOut.String(), `unknown command "bogus"`) {
		t.Fatalf("stderr=%q", errOut.String())
	}

	errOut.Reset()
	if code := Run(nil, nil, &out, &errOut, Env{}); code != 0 {
		t.Fatalf("code=%d", code)
	}
}
