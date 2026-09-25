package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
)

// failingDeleteKeychain behaves like fakeKeychain but cannot delete.
type failingDeleteKeychain struct {
	*fakeKeychain
	deleteErr error
}

func (k *failingDeleteKeychain) Delete(context.Context, string) error { return k.deleteErr }

// Regression: CLI correctness review, 2026-09 (e35b8ac).
func TestUninstallPurgeLeavesNoFilesOrDirectory(t *testing.T) {
	home, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, true, false, t.TempDir()))
	var stdout, stderr bytes.Buffer
	if code := runUninstallCommand([]string{"--delete-local-data"}, strings.NewReader("y\ny\n"), &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(home)
		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("data directory survived the purge (err=%v) with %q", err, names)
	}
}

// The purge must know every entry the running system creates, not only the
// ones setup does: the collector's directories (created by every
// state.Open), the lineage ledger and reader cache (created on
// first use), and the diagnostics lock (created by the first diagnostic a
// hook records).
//
// Regression: CLI correctness review, 2026-09 (e35b8ac).
func TestUninstallPurgeRemovesCollectorAndDiagnosticState(t *testing.T) {
	home, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, true, false, t.TempDir()))
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "scan-signatures", "session.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSuperseded("session", "sessions/codex/session/source.old.jsonl.gz", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.OpenMetadataCache(home); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRemoval("codex", "native", state.RemovalReasonRetention, time.Now()); err != nil {
		t.Fatal(err)
	}
	cfg, _, _ := config.Load(home)
	if err := recordCaptureDiagnostic(home, captureDiagnostic{Code: diagnosticSetupInProgress, Harness: "codex", ProjectRoot: cfg.Archive.Projects[0].Root, ObservedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"scan-signatures", "superseded", "forgotten", "cache", diagnosticsLockName, "capture-diagnostics.json"} {
		if _, err := os.Stat(filepath.Join(home, name)); err != nil {
			t.Fatalf("test precondition: %s was not created: %v", name, err)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := runUninstallCommand([]string{"--delete-local-data"}, strings.NewReader("y\ny\n"), &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(home)
		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("data directory survived the purge (err=%v) with %q", err, names)
	}
}

// A Keychain that refuses to delete must not strand the purge halfway: local
// files are still removed, and the items left in the Keychain are named with
// the recovery for that failure, because nothing else records them once
// config.json is gone.
//
// Regression: CLI correctness review, 2026-09 (e35b8ac).
func TestUninstallPurgeContinuesPastKeychainFailure(t *testing.T) {
	for name, keychainFor := range map[string]func(*fakeKeychain) (credentials.CredentialStore, error){
		"delete refused": func(k *fakeKeychain) (credentials.CredentialStore, error) {
			return &failingDeleteKeychain{fakeKeychain: k, deleteErr: credentials.ErrKeychainLocked}, nil
		},
		"keychain unavailable": func(*fakeKeychain) (credentials.CredentialStore, error) {
			return nil, credentials.ErrKeychainLocked
		},
	} {
		t.Run(name, func(t *testing.T) {
			keychain := newFakeKeychain()
			home, _, env := installedFixture(t, keychain, r2SetupInput(t.TempDir(), "supersecret"))
			cfg, _, _ := config.Load(home)
			ref := cfg.Storage.R2CredentialRef
			env.Keychain = func() (credentials.CredentialStore, error) { return keychainFor(keychain) }

			var stdout, stderr bytes.Buffer
			code := runUninstallCommand([]string{"--delete-local-data"}, strings.NewReader("y\ny\n"), &stdout, &stderr, env)
			if code != 1 {
				t.Fatalf("an undeleted credential must make uninstall incomplete: code=%d stderr=%s", code, stderr.String())
			}
			message := stderr.String()
			// The recovery names the service, the count, and the exact
			// command per opaque reference; it must not send the user to
			// sync or setup, which no longer exist after a purge.
			for _, want := range []string{
				`1 stored credential(s) could not be deleted from Keychain service "agent-archive"`,
				"security delete-generic-password -s agent-archive -a " + ref,
				"Keychain Access", "Unlock the login Keychain",
			} {
				if !strings.Contains(message, want) {
					t.Errorf("stderr must mention %q:\n%s", want, message)
				}
			}
			for _, reject := range []string{"agent-archive sync", "agent-archive setup"} {
				if strings.Contains(message, reject) {
					t.Errorf("stderr must not suggest %q after a purge:\n%s", reject, message)
				}
			}
			if !strings.HasPrefix(ref, "setup-") || len(ref) != len("setup-")+32 {
				t.Fatalf("credential reference %q is not the opaque setup-<hex> form the output relies on", ref)
			}
			if strings.Contains(stdout.String()+message, "supersecret") {
				t.Fatal("uninstall output contains a secret")
			}
			if _, err := os.Stat(home); !os.IsNotExist(err) {
				t.Fatalf("local files were not removed after the Keychain failure: %v", err)
			}
		})
	}
}

func TestUninstallPurgeRequiresSecondConfirmation(t *testing.T) {
	home, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, t.TempDir()))
	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall", "--delete-local-data"}, strings.NewReader("y\nn\n"), &out, &errOut, env); code != 0 {
		t.Fatal(errOut.String())
	}
	cfg, found, _ := config.Load(home)
	if !found || !cfg.Archive.Enabled {
		t.Fatal("declining purge altered installation")
	}
}

func TestPurgeConfirmationDoesNotBlockCapture(t *testing.T) {
	home, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, t.TempDir()))
	input := &checkingReader{Reader: strings.NewReader("y\nn\n"), check: func() {
		// Prompts may wait indefinitely; only the wizard lock may be held here.
		unlock, err := local.Lock(home)
		if err != nil {
			t.Fatal("prompt held collector lock")
		}
		unlock()
	}}
	env.IsTerminal = func(stream any) bool { return stream == any(input) }
	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall", "--delete-local-data"}, input, &out, &errOut, env); code != 0 {
		t.Fatal(errOut.String())
	}
}

type checkingReader struct {
	*strings.Reader
	check func()
}

func (r *checkingReader) Read(b []byte) (int, error) { r.check(); return r.Reader.Read(b[:1]) }
