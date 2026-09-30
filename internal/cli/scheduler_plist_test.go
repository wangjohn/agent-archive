package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// Characterization of the macOS scheduler (PR 5a-0 of
// dev/proposals/platform-abstraction.md): the collector's LaunchAgent plist is
// on disk in every existing installation and launchd loads it, so its bytes,
// its label and its location may not change when the code that writes it moves.
// Setup runs through its own commands, so this pins what a user's disk holds,
// not which function wrote it, and names no scheduler code;
// scheduler_plist_internal_test.go holds what does. The golden files are in
// testdata/scheduler/plists, with the folders of the run named @DATA_HOME@,
// @EXECUTABLE@, @BIN_DIR@ and @AWS_DIR@ (the folder above a data directory
// that must be escaped @TMP@), and a non-default label's hash @HASH@ (the hash
// is derived apart, below).

// wantCollectorLabel is the launchd label of the collector of a data
// directory, derived here rather than asked of the code under test: the
// default installation's is com.agent-archive.collector, and any other's adds
// a dot and the first 12 hex digits of the SHA-256 of the directory with its
// symlinks resolved.
func wantCollectorLabel(t *testing.T, dataHome string, isDefault bool) string {
	t.Helper()
	if isDefault {
		return "com.agent-archive.collector"
	}
	return "com.agent-archive.collector." + labelHash(canonical(t, dataHome))
}

func labelHash(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:])[:12]
}

// canonical is path with its symlinks resolved (t.TempDir is under a symlink
// on macOS).
func canonical(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	must(t, err)
	return resolved
}

// xmlText is s as the plist's XML writes it.
var xmlText = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;")

// plistStorage is the storage the setup chose, which decides the environment
// the plist carries.
type plistStorage int

const (
	plistR2            plistStorage = iota // credentials in the Keychain: no environment
	plistS3                                // S3 with nothing in the shell: only a PATH
	plistS3Environment                     // S3 with AWS files, an endpoint, a proxy and a PATH in the shell
)

// awsEnvironment is what setup carries into the plist from a shell that sets
// the AWS files, an endpoint, a proxy and a PATH.
var awsEnvironment = []string{"AWS_CONFIG_FILE", "AWS_ENDPOINT_URL_S3", "AWS_SHARED_CREDENTIALS_FILE", "HTTPS_PROXY", "PATH"}

// The plist setup writes, for each storage and for the default installation
// and another data directory, byte for byte.
func TestCollectorPlistBytes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		isDefault bool
		escaped   bool // a data directory and a proxy the XML must escape
		storage   plistStorage
		wantEnv   []string // the environment variables the plist carries besides AGENT_ARCHIVE_HOME
	}{
		{"default-r2", true, false, plistR2, nil},
		{"default-s3", true, false, plistS3, []string{"PATH"}},
		{"default-s3-environment", true, false, plistS3Environment, awsEnvironment},
		{"other-directory-r2", false, false, plistR2, nil},
		{"other-directory-s3", false, false, plistS3, []string{"PATH"}},
		{"other-directory-s3-environment", false, false, plistS3Environment, awsEnvironment},
		{"other-directory-escaped", false, true, plistS3Environment, awsEnvironment},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			account, userHome, project, tmp := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
			// The default data directory is new to setup; another one
			// already exists.
			home := filepath.Join(account, ".local", "share", "agent-archive")
			if !tc.isDefault {
				home = filepath.Join(tmp, "data")
				if tc.escaped {
					home = filepath.Join(tmp, `a & <b> "c"`)
				}
				must(t, os.Mkdir(home, 0o700))
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
				proxy := "http://proxy.internal.example:3128"
				if tc.escaped {
					proxy = "http://proxy.internal.example:3128/?a=1&b=<2>"
				}
				env.LookupEnv = shellEnvironment(map[string]string{
					"AWS_CONFIG_FILE":             configFile,
					"AWS_SHARED_CREDENTIALS_FILE": credentialsFile,
					"AWS_ENDPOINT_URL_S3":         "https://s3.internal.example",
					"HTTPS_PROXY":                 proxy,
					"PATH":                        bin + ":/usr/bin",
				})
				input = s3SetupInput("b", "us-east-1", "vault", false, true, false, project)
			case plistS3:
			}
			setupRun(t, env, input, 0)

			// The location: the file name is the label, in the user's
			// LaunchAgents folder, which holds nothing else, private to the
			// user.
			label := wantCollectorLabel(t, home, tc.isDefault)
			agents := filepath.Join(userHome, "Library", "LaunchAgents")
			entries, err := os.ReadDir(agents)
			must(t, err)
			if len(entries) != 1 || entries[0].Name() != label+".plist" {
				t.Fatalf("LaunchAgents holds %v, want only %s.plist", entries, label)
			}
			plistPath := filepath.Join(agents, label+".plist")
			info, err := os.Stat(plistPath)
			must(t, err)
			if info.Mode().Perm() != 0o600 {
				t.Errorf("plist mode %v, want 0600", info.Mode().Perm())
			}
			data, err := os.ReadFile(plistPath)
			must(t, err)
			// The environment, apart from the golden file, so that an update
			// of it cannot drop what the storage needs: R2 keeps its
			// credentials in the Keychain, and needs only the data directory.
			if got := strings.Count(string(data), "<key>"); got != 9+len(tc.wantEnv) {
				t.Errorf("the plist has %d keys, want %d", got, 9+len(tc.wantEnv))
			}
			for _, name := range append([]string{"AGENT_ARCHIVE_HOME"}, tc.wantEnv...) {
				if !strings.Contains(string(data), "<key>"+name+"</key>") {
					t.Errorf("the plist does not set %s", name)
				}
			}
			executable, err := env.Executable()
			must(t, err)
			pairs := []string{xmlText.Replace(executable), "@EXECUTABLE@", xmlText.Replace(binDir), "@BIN_DIR@", xmlText.Replace(awsDir), "@AWS_DIR@"}
			if tc.escaped {
				// So that the escaping shows, only the folder above it is named.
				pairs = append(pairs, xmlText.Replace(tmp), "@TMP@")
			} else {
				pairs = append(pairs, xmlText.Replace(home), "@DATA_HOME@")
			}
			if !tc.isDefault {
				pairs = append(pairs, strings.TrimPrefix(label, "com.agent-archive.collector."), "@HASH@")
			}
			text := strings.NewReplacer(pairs...).Replace(string(data))
			golden.Check(t, filepath.Join("testdata", "scheduler", "plists", tc.name+".plist"), []byte(text))
		})
	}
}
