package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestSyncEndToEndFromHookThroughPublish(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dir := t.TempDir()
	setUpTestConfig(t, home, dir, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	transcript := writeCodexTranscript(t, dir)
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": dir, "transcript_path": transcript}
	if err := capture.HandleEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}

	env := testEnv(t, home, now)
	var stdout, stderr bytes.Buffer
	if code := runSyncCommand(nil, &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "1 published") {
		t.Fatalf("stdout=%s", stdout.String())
	}

	stdout.Reset()
	if code := runStatusCommand(nil, &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Pending:       0") {
		t.Fatalf("status did not reflect the publish: %s", stdout.String())
	}
}

func TestSyncReportsNotSetUp(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := testEnv(t, home, time.Now())
	var stdout, stderr bytes.Buffer
	if code := runSyncCommand(nil, &stdout, &stderr, env); code != 1 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(stderr.String(), "run `agent-archive setup`") {
		t.Fatalf("stderr=%s", stderr.String())
	}
}

func TestSyncReportsLockContentionAsAnError(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	unlock, err := local.Lock(home)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	env := testEnv(t, home, time.Now())
	var out, errOut bytes.Buffer
	if code := runSyncCommand(nil, &out, &errOut, env); code != 1 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(errOut.String(), "holds the collector lock; retry when it finishes") {
		t.Fatalf("stderr=%s", errOut.String())
	}
}

func TestSyncRunsRetentionSweepAndDeletesExpiredSession(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dir := t.TempDir()
	setUpTestConfig(t, home, dir, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	// setUpTestConfig doesn't set RetentionDays; give it a short window here.
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RetentionDays = 1
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	transcript := writeCodexTranscript(t, dir)
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": dir, "transcript_path": transcript}
	if err := capture.HandleEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}

	var mem *storagetest.MemoryStore
	env := testEnv(t, home, now)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		if mem == nil {
			mem = storagetest.NewMemoryStore()
		}
		return mem, nil
	}
	var stdout, stderr bytes.Buffer
	if code := runSyncCommand(nil, &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}

	// Two days later, well past the 1-day retention window.
	later := now.Add(48 * time.Hour)
	storageClockFollows(t, &env, func() time.Time { return later })
	env2 := env
	env2.Now = func() time.Time { return later }
	stdout.Reset()
	stderr.Reset()
	if code := runSyncCommand(nil, &stdout, &stderr, env2); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}

	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) != 0 {
		t.Fatalf("expected the session to have aged out: %#v", regs)
	}
}

// failingDeleteStore fails every Delete, used to force retention.Sweep to
// report a per-session error without needing to know its internal object
// key naming.
type failingDeleteStore struct{ storage.ObjectStore }

func (f failingDeleteStore) Delete(ctx context.Context, key string) error {
	return errors.New("simulated delete failure")
}

func TestSyncSurfacesRetentionErrorsInResultAndStatus(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dir := t.TempDir()
	setUpTestConfig(t, home, dir, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RetentionDays = 1
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	transcript := writeCodexTranscript(t, dir)
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": dir, "transcript_path": transcript}
	if err := capture.HandleEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}

	var mem *storagetest.MemoryStore
	env := testEnv(t, home, now)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		if mem == nil {
			mem = storagetest.NewMemoryStore()
		}
		return mem, nil
	}
	var stdout, stderr bytes.Buffer
	if code := runSyncCommand(nil, &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}

	// Two days later, well past the 1-day retention window, but every
	// delete this pass attempts now fails.
	later := now.Add(48 * time.Hour)
	storageClockFollows(t, &env, func() time.Time { return later })
	env2 := env
	env2.Now = func() time.Time { return later }
	env2.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		return failingDeleteStore{mem}, nil
	}
	stdout.Reset()
	stderr.Reset()
	code := runSyncCommand(nil, &stdout, &stderr, env2)
	if code != 1 {
		t.Fatalf("expected sync to report the retention failure as an error: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "retention:") {
		t.Fatalf("expected sync's report to mention the retention failure: stderr=%s", stderr.String())
	}

	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	status, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.LastError, "failed to scan, publish, or clean up") {
		t.Fatalf("expected status.LastError to reflect the retention failure, got %q", status.LastError)
	}
	// The session must still be registered locally: a failed delete must
	// never be treated as if it had succeeded.
	regs, err := store.LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) != 1 {
		t.Fatalf("expected the session to remain registered after a failed retention delete: %#v", regs)
	}
}

// Regression: CLI correctness review, 2026-09 (e35b8ac).
func TestSyncAndStatusGiveTheKeychainRecovery(t *testing.T) {
	t.Parallel()
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
