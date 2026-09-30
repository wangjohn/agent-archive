package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// Characterization of the macOS scheduler (PR 5a-0 of
// dev/proposals/platform-abstraction.md): the collector's LaunchAgent plist is
// on disk in every existing installation and launchd loads it, so its bytes,
// its label and its location may not change when the code that writes it moves.
// Setup runs through its own commands, so this pins what a user's disk holds,
// not which function wrote it; the golden files are in
// testdata/scheduler/plists, with the folders of the run named @DATA_HOME@,
// @EXECUTABLE@, @BIN_DIR@ and @AWS_DIR@ and a non-default label's hash @HASH@
// (the hash is checked apart, below).

// plistStorage is the storage the setup chose, which decides the environment
// the plist carries.
type plistStorage int

const (
	plistR2            plistStorage = iota // credentials in the Keychain: no environment
	plistS3                                // S3 with nothing in the shell: only a PATH
	plistS3Environment                     // S3 with AWS files, an endpoint, a proxy and a PATH in the shell
)

// The plist setup writes, for each storage and for the default installation
// and another data directory, byte for byte.
func TestCollectorPlistBytes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		isDefault  bool
		storage    plistStorage
		wantEnvKey string // an environment variable the plist must carry, "" for none
	}{
		{"default-r2", true, plistR2, ""},
		{"default-s3", true, plistS3, "PATH"},
		{"default-s3-environment", true, plistS3Environment, "AWS_CONFIG_FILE"},
		{"other-directory-r2", false, plistR2, ""},
		{"other-directory-s3", false, plistS3, "PATH"},
		{"other-directory-s3-environment", false, plistS3Environment, "AWS_CONFIG_FILE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			account, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
			home := t.TempDir()
			if tc.isDefault {
				home = filepath.Join(account, ".local", "share", "agent-archive")
			}
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			env.AccountHome = func() (string, error) { return account, nil }
			input := s3SetupInput("b", "us-east-1", "p", false, true, false, project)
			awsDir, binDir := "@none@", "@none@"
			switch tc.storage {
			case plistR2:
				input = r2SetupInput(project, "the-secret")
			case plistS3Environment:
				configFile, credentialsFile, bin := awsFixture(t)
				awsDir, binDir = filepath.Dir(configFile), bin
				env.LookupEnv = shellEnvironment(map[string]string{
					"AWS_CONFIG_FILE":             configFile,
					"AWS_SHARED_CREDENTIALS_FILE": credentialsFile,
					"AWS_ENDPOINT_URL_S3":         "https://s3.internal.example",
					"HTTPS_PROXY":                 "http://proxy.internal.example:3128",
					"PATH":                        bin + ":/usr/bin",
				})
				input = s3SetupInput("b", "us-east-1", "vault", false, true, false, project)
			case plistS3:
			}
			setupRun(t, env, input, 0)

			plistPath := env.installation(home, userHome).collectorPlist()
			label := hooks.LaunchLabel
			if !tc.isDefault {
				sum := sha256.Sum256([]byte(local.CanonicalPath(home)))
				label += "." + hex.EncodeToString(sum[:])[:12]
			}
			// The location and the label: the file name is the label, in the
			// user's LaunchAgents folder, private to the user.
			if want := filepath.Join(userHome, "Library", "LaunchAgents", label+".plist"); plistPath != want {
				t.Fatalf("plist at %s, want %s", plistPath, want)
			}
			info, err := os.Stat(plistPath)
			must(t, err)
			if info.Mode().Perm() != 0o600 {
				t.Errorf("plist mode %v, want 0600", info.Mode().Perm())
			}
			data, err := os.ReadFile(plistPath)
			must(t, err)
			environment, err := hooks.LaunchAgentEnvironment(data)
			must(t, err)
			// R2 keeps its credentials in the Keychain: only the data directory.
			if _, has := environment[tc.wantEnvKey]; (tc.wantEnvKey != "" && !has) || (tc.wantEnvKey == "" && len(environment) != 1) {
				t.Fatalf("collector environment %v, want %q", environment, tc.wantEnvKey)
			}
			executable, _ := env.executable()
			pairs := []string{executable, "@EXECUTABLE@", binDir, "@BIN_DIR@", awsDir, "@AWS_DIR@", home, "@DATA_HOME@"}
			if !tc.isDefault {
				pairs = append(pairs, strings.TrimPrefix(label, hooks.LaunchLabel+"."), "@HASH@")
			}
			text := strings.NewReplacer(pairs...).Replace(string(data))
			golden.Check(t, filepath.Join("testdata", "scheduler", "plists", tc.name+".plist"), []byte(text))
		})
	}
}
