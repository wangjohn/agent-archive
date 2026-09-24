package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// A scheduled pass that meets a corrupt state file still publishes the rest,
// names the quarantined file in status, and keeps collector-error.log (which
// launchd appends to and never rotates) bounded.
func TestCollectPassReportsQuarantinedStateAndTrimsErrorLog(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	setUpTestConfig(t, home, dir, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	transcript := writeCodexTranscript(t, dir)
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": dir, "transcript_path": transcript}
	if err := handleHookEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "registrations", "broken.json"), []byte(`{"archive_session_id":`), 0o600); err != nil {
		t.Fatal(err)
	}
	errorLog := filepath.Join(home, "collector-error.log")
	if err := os.WriteFile(errorLog, bytes.Repeat([]byte("agent-archive: collect: old failure\n"), (errorLogMaxBytes/36)+1), 0o600); err != nil {
		t.Fatal(err)
	}

	env := testEnv(t, home, now)
	var out, errOut bytes.Buffer
	if code := runCollectCommand(nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}

	store, err := collector.NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	status, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.SessionIssues["broken"] != "local_state_unreadable" || len(status.QuarantinedFiles) != 1 || !strings.HasPrefix(status.QuarantinedFiles[0], "registrations/broken.json.") {
		t.Fatalf("status = %#v", status)
	}
	if status.LastPublishedAt.IsZero() {
		t.Fatal("the readable session was not published")
	}
	if info, err := os.Stat(errorLog); err != nil || info.Size() > errorLogKeepBytes {
		t.Fatalf("collector-error.log was not trimmed: %v %v", info.Size(), err)
	}
}

// collectFixture sets up one hook-registered codex session and an Env whose
// passes all publish to the same in-memory bucket.
func collectFixture(t *testing.T, now time.Time) (home string, env Env, remote *storage.MemoryStore) {
	t.Helper()
	home = t.TempDir()
	dir := t.TempDir()
	setUpTestConfig(t, home, dir, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": dir, "transcript_path": writeCodexTranscript(t, dir)}
	if err := handleHookEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}
	remote = storage.NewMemoryStore()
	env = testEnv(t, home, now)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	return home, env, remote
}

func theRegistration(t *testing.T, home string) archive.SessionRegistration {
	t.Helper()
	regs, err := collector.OpenLocalStoreReadOnly(home).LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("registrations = %#v %v", regs, err)
	}
	return regs[0]
}

// A registration that cannot be read at all (a root-owned file left by a
// sudo run, say) fails only its own session: read-back verification skips
// it, and the retention sweep still runs.
func TestCollectPassSweepsDespiteUnreadableRegistration(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	home, env, _ := collectFixture(t, now)
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RetentionDays = 1
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	id := theRegistration(t, home).ArchiveSessionID

	locked := filepath.Join(home, "registrations", "locked.json")
	if err := os.WriteFile(locked, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	env.Now = func() time.Time { return now.Add(72 * time.Hour) }
	result, err := runOnePass(env, false)
	if err != nil {
		t.Fatalf("pass failed: %v", err)
	}
	if result.Errors["locked"] == nil {
		t.Fatalf("the unreadable registration was not reported: %#v", result.Errors)
	}
	if _, err := os.Stat(filepath.Join(home, "registrations", id+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the retention sweep did not run: expired session still registered (%v)", err)
	}
}

// Past the soft deadline a pass starts no new session. Sessions it did not
// reach keep their work, and nothing is reported as failing.
func TestCollectPassSoftDeadlineStartsNoNewSession(t *testing.T) {
	previous := collectSoftDeadline
	collectSoftDeadline = 0
	t.Cleanup(func() { collectSoftDeadline = previous })
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	home, env, _ := collectFixture(t, now)

	result, err := runOnePass(env, false)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 || result.Scanned != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	status, err := collector.OpenLocalStoreReadOnly(home).LoadStatus()
	if err != nil || status.LastError != "" {
		t.Fatalf("status = %#v %v", status, err)
	}

	collectSoftDeadline = previous
	if result, err := runOnePass(env, false); err != nil || len(result.Published) != 1 {
		t.Fatalf("the next pass did not pick the session up: %#v %v", result, err)
	}
}

// Read-back verification checks the remote metadata against the source
// reference recorded at upload, so a cached bundle this build can no longer
// serialize (a source schema bump) still verifies.
func TestReadBackUsesRecordedSourceAfterSchemaBump(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	home, env, remote := collectFixture(t, now)
	if result, err := runOnePass(env, false); err != nil || len(result.Published) != 1 {
		t.Fatalf("%#v %v", result, err)
	}
	id := theRegistration(t, home).ArchiveSessionID
	publishedPath := filepath.Join(home, "published", id+".json")
	var state map[string]any
	if err := local.Read(publishedPath, &state); err != nil {
		t.Fatal(err)
	}
	state["bundle"].(map[string]any)["schema_version"] = 1
	if err := local.Write(publishedPath, state); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(verificationPath(home, id)); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	store, err := collector.NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	if summary, err := verifyPublications(home, cfg, env, store, remote); err != nil || summary.Verified != 1 {
		t.Fatalf("summary = %#v %v", summary, err)
	}
}

// A read-back verification that fails (here: one session's published state
// cannot be read) is reported, but only after the retention sweep has run:
// another, expired session is still cleaned up in the same pass.
func TestCollectPassSweepsWhenVerificationFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	home, env, _ := collectFixture(t, now)
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RetentionDays = 3
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	expired := theRegistration(t, home).ArchiveSessionID

	// A second session, published two days later, so it is not yet expired
	// when the first one is.
	later := now.Add(48 * time.Hour)
	dir := t.TempDir()
	cfg, _, _ = config.Load(home)
	cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{ProjectID: archive.ProjectID(dir), Root: dir, Included: true, ActivatedAt: now})
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-2", "cwd": dir, "transcript_path": writeCodexTranscript(t, dir)}
	if err := handleHookEvent(home, "codex", payload, later); err != nil {
		t.Fatal(err)
	}
	env.Now = func() time.Time { return later }
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	regs, err := collector.OpenLocalStoreReadOnly(home).LoadRegistrations()
	if err != nil || len(regs) != 2 {
		t.Fatalf("registrations = %v %v", regs, err)
	}
	unreadable := regs[0].ArchiveSessionID
	if unreadable == expired {
		unreadable = regs[1].ArchiveSessionID
	}
	// Force a fresh read-back of that session, which cannot read its state.
	if err := os.Remove(verificationPath(home, unreadable)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(home, "published", unreadable+".json"), 0); err != nil {
		t.Fatal(err)
	}

	env.Now = func() time.Time { return now.Add(96 * time.Hour) }
	_, err = runOnePass(env, false)
	if err == nil || !strings.Contains(err.Error(), "read-back verification") {
		t.Fatalf("pass error = %v, want the verification failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, "registrations", expired+".json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the retention sweep did not run: expired session still registered (%v)", statErr)
	}
	status, statusErr := collector.OpenLocalStoreReadOnly(home).LoadStatus()
	if statusErr != nil || !strings.Contains(status.LastError, "read-back verification") {
		t.Fatalf("status = %#v %v", status, statusErr)
	}
}
