package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestFeedbackFileIsFilteredBeforeRequestPersistence(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, now.Add(-time.Hour))
	if err := capture.HandleEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": project}, now); err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(home)
	regs, _ := store.LoadRegistrations()
	path := filepath.Join(t.TempDir(), "feedback.txt")
	if err := os.WriteFile(path, []byte("useful correction; password=synthetic-secret-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"feedback", regs[0].ArchiveSessionID, "--file", path}, nil, &stdout, &stderr, testEnv(t, home, now.Add(time.Minute)))
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	requests, err := store.LoadRequests()
	if err != nil || len(requests) != 1 || len(requests[0].HookEvidence) != 2 {
		t.Fatalf("requests=%#v err=%v", requests, err)
	}
	var evidence archive.SupplementalEvidence
	for _, item := range requests[0].HookEvidence {
		if item.Kind == archive.EvidenceKindExplicitFeedback {
			evidence = item
		}
	}
	text, _ := evidence.Payload["text"].(string)
	if evidence.Provenance != "user:agent-archive-feedback-file" || evidence.Payload["redacted"] != true || strings.Contains(text, "synthetic-secret-value") || !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("evidence=%#v", evidence)
	}
	if gaps, _ := evidence.Payload["gaps"].([]any); len(gaps) != 1 || gaps[0] != "sensitive_content_redacted" || evidence.Payload["truncated"] != nil {
		t.Fatalf("gap labels=%#v", evidence.Payload)
	}
	encoded, err := json.Marshal(requests)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), path) {
		t.Fatalf("local input path persisted: %s", encoded)
	}
}

func TestFeedbackRejectsMissingUnknownAndOversizeInput(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	now := time.Now()
	setUpTestConfig(t, home, project, now.Add(-time.Hour))
	large := filepath.Join(t.TempDir(), "large.txt")
	if err := os.WriteFile(large, bytes.Repeat([]byte("x"), maxFeedbackBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"feedback", "missing"}, {"feedback", "missing", "--file", large}} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, nil, &stdout, &stderr, testEnv(t, home, now)); code == 0 {
			t.Fatalf("args=%v stdout=%s", args, stdout.String())
		}
	}
}

func TestFeedbackRejectsSessionExcludedByCurrentSetup(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	now := time.Now().UTC()
	setUpTestConfig(t, home, project, now.Add(-time.Hour))
	if err := capture.HandleEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": project}, now); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	registrations, err := store.LoadRegistrations()
	if err != nil || len(registrations) != 1 {
		t.Fatalf("registrations=%#v err=%v", registrations, err)
	}
	cfg, found, err := config.Load(home)
	if err != nil || !found {
		t.Fatalf("config found=%v err=%v", found, err)
	}
	cfg.Archive.Projects[0].Included = false
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "feedback.txt")
	if err := os.WriteFile(path, []byte("useful correction"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"feedback", registrations[0].ArchiveSessionID, "--file", path}, nil, &stdout, &stderr, testEnv(t, home, now.Add(time.Minute)))
	if code == 0 || !strings.Contains(stderr.String(), "no longer eligible") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	requests, err := store.LoadRequests()
	if err != nil || len(requests) != 1 || len(requests[0].HookEvidence) != 1 || requests[0].HookEvidence[0].Kind != archive.EvidenceKindLifecycleHook || !requests[0].Deferred {
		t.Fatalf("requests=%#v err=%v", requests, err)
	}
}

// Feedback queued while capture is paused says when it will be published.
func TestFeedbackWhilePausedSaysWhen(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	env := testEnv(t, home, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	start := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": project, "transcript_path": writeTestTranscript(t, "t.jsonl", "")}
	if err := capture.HandleEvent(home, "claude", start, env.now()); err != nil {
		t.Fatal(err)
	}
	regs, _ := state.OpenReadOnly(home).LoadRegistrations()
	if len(regs) != 1 {
		t.Fatalf("registrations %d", len(regs))
	}
	file := filepath.Join(t.TempDir(), "feedback.txt")
	must(t, os.WriteFile(file, []byte("worked well"), 0600))
	run := func() string {
		var out, errOut bytes.Buffer
		if code := Run([]string{"feedback", regs[0].ArchiveSessionID, "--file", file}, nil, &out, &errOut, env); code != 0 {
			t.Fatalf("exit %d: %s", code, &errOut)
		}
		return out.String()
	}
	if strings.Contains(run(), "paused") {
		t.Fatal("mentions pause while not paused")
	}
	var errOut bytes.Buffer
	if code := runPauseCommand(&bytes.Buffer{}, &errOut, env, true); code != 0 {
		t.Fatal(errOut.String())
	}
	if !strings.Contains(run(), "agent-archive resume") {
		t.Fatal("paused feedback does not say when it is published")
	}
}
