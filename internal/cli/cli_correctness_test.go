package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// failingDeleteKeychain behaves like fakeKeychain but cannot delete.
type failingDeleteKeychain struct {
	*fakeKeychain
	deleteErr error
}

func (k *failingDeleteKeychain) Delete(context.Context, string) error { return k.deleteErr }

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
// collector.NewLocalStore), the lineage ledger and reader cache (created on
// first use), and the diagnostics lock (created by the first diagnostic a
// hook records).
func TestUninstallPurgeRemovesCollectorAndDiagnosticState(t *testing.T) {
	home, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, true, false, t.TempDir()))
	store, err := collector.NewLocalStore(home)
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
	cfg, _, _ := config.Load(home)
	if err := recordCaptureDiagnostic(home, captureDiagnostic{Code: diagnosticSetupInProgress, Harness: "codex", ProjectRoot: cfg.Archive.Projects[0].Root, ObservedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"scan-signatures", "superseded", "cache", diagnosticsLockName, "capture-diagnostics.json"} {
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

func TestStatusChecksHooksAgainstTheInstalledExecutable(t *testing.T) {
	home, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, true, false, t.TempDir()))
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := env.executable()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InstalledExecutable != installed {
		t.Fatalf("setup recorded executable %q, want %q", cfg.InstalledExecutable, installed)
	}

	// status run through a different path (a shim, a symlink, a copy).
	env.Executable = func() (string, error) { return "/usr/local/bin/agent-archive", nil }
	hooksByApp := func() map[string]string {
		t.Helper()
		view, err := readStatus(env)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, app := range view.Apps {
			out[app.Name] = app.Hooks
		}
		return out
	}
	for app, state := range hooksByApp() {
		if state != "installed" {
			t.Fatalf("%s hooks = %q from a different executable path", app, state)
		}
	}

	// A configuration written before the field existed falls back to the
	// running executable, which here is not the installed one.
	cfg.InstalledExecutable = ""
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	for app, state := range hooksByApp() {
		if state != "missing or incomplete" {
			t.Fatalf("fallback: %s hooks = %q", app, state)
		}
	}
	env.Executable = func() (string, error) { return installed, nil }
	for app, state := range hooksByApp() {
		if state != "installed" {
			t.Fatalf("fallback with the installed path: %s hooks = %q", app, state)
		}
	}
}

func TestSyncAndStatusGiveTheKeychainRecovery(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{credentials.ErrKeychainLocked, "Unlock the login Keychain"},
		{credentials.ErrKeychainItemNotFound, "save it again"},
		{&credentials.KeychainStatusError{Status: -34018}, "could not be read"},
	} {
		_, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, false, false, t.TempDir()))
		env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
			return nil, fmt.Errorf("load R2 credentials: %w", c.err)
		}
		var stdout, stderr bytes.Buffer
		if code := runSyncCommand(nil, &stdout, &stderr, env); code != 1 {
			t.Fatalf("%v: sync code=%d", c.err, code)
		}
		if !strings.Contains(stderr.String(), c.want) {
			t.Fatalf("%v: sync stderr lacks %q:\n%s", c.err, c.want, stderr.String())
		}
		view, err := readStatus(env)
		if err != nil {
			t.Fatal(err)
		}
		if view.State != "Needs attention" || !strings.Contains(view.Next, c.want) {
			t.Fatalf("%v: status = %q, next = %q", c.err, view.State, view.Next)
		}
	}

	// Any other storage failure keeps the general advice.
	_, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, false, false, t.TempDir()))
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		return nil, errors.New("dial tcp: connection refused")
	}
	var stdout, stderr bytes.Buffer
	runSyncCommand(nil, &stdout, &stderr, env)
	if strings.Contains(stderr.String(), "Keychain") {
		t.Fatalf("a network failure was given Keychain advice:\n%s", stderr.String())
	}
	if view, err := readStatus(env); err != nil || !strings.HasPrefix(view.Next, "Check storage access") {
		t.Fatalf("general failure next = %q err=%v", view.Next, err)
	}
}

func TestStatusExplainsWhyHookTrustIsUnknown(t *testing.T) {
	_, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, false, false, t.TempDir()))
	var stdout, stderr bytes.Buffer
	if code := runStatusCommand(nil, &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "granted inside the app and is not observable") {
		t.Fatalf("status does not explain unknown trust:\n%s", stdout.String())
	}
	stdout.Reset()
	if code := runStatusCommand([]string{"--json"}, &stdout, &stderr, env); code != 0 || !strings.Contains(stdout.String(), `"trust": "unknown"`) {
		t.Fatalf("the JSON value must stay unknown: code=%d\n%s", code, stdout.String())
	}
}
