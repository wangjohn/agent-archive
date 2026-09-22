package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// publishedThroughSync sets up a project, registers and publishes one Codex
// session into a shared in-memory bucket, and returns the environment, home,
// transcript path, and bucket. Retention is set to 90 days.
func publishedThroughSync(t *testing.T, now time.Time) (Env, string, string, storage.ObjectStore) {
	t.Helper()
	home, project := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), now)
	setupRun(t, env, s3SetupInput("bucket", "us-east-1", "profile", true, false, false, project), 0)
	bucket := storage.NewMemoryStore()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return bucket, nil }
	path := writeCodexTranscript(t, project)
	if err := handleHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native", "cwd": project, "transcript_path": path}, now); err != nil {
		t.Fatal(err)
	}
	result, err := runOnePass(env, false)
	if err != nil || len(result.Published) != 1 {
		t.Fatalf("setup publish: %+v %v", result, err)
	}
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RetentionDays = 90
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	return env, home, path, bucket
}

// Applications delete their own transcripts. That is a capture gap, reported
// as such with its own explanation, and never a collector error.
func TestStatusReportsDeletedTranscriptAsGapNotError(t *testing.T) {
	now := time.Now().UTC()
	env, _, path, _ := publishedThroughSync(t, now)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	env.Now = func() time.Time { return now.Add(time.Hour) }
	for pass := 0; pass < 2; pass++ {
		if result, err := runOnePass(env, false); err != nil || len(result.Errors) != 0 {
			t.Fatalf("pass %d: %+v %v", pass, result, err)
		}
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.Collector.LastError != "" || view.Collector.PendingCount != 0 {
		t.Fatalf("a deleted transcript surfaced as an error: %+v", view.Collector)
	}
	gaps := view.Apps[0].CaptureGaps
	if len(gaps) != 1 || gaps[0].Code != string(collector.BlockedReasonTranscriptMissing) || !strings.Contains(gaps[0].Detail, "resumes by itself") {
		t.Fatalf("gaps=%+v", gaps)
	}
	var out strings.Builder
	if code := runStatusCommand(nil, &out, os.Stderr, env); code != 0 || strings.Contains(out.String(), "Last error") || !strings.Contains(out.String(), "1 with a capture gap") {
		t.Fatalf("status exit=%d output=%s", code, out.String())
	}
}
