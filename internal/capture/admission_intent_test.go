package capture

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestAdmissionIntentReplayIsIdempotentAndExpires(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	payload := claudeStart(project, "native-1", "startup", "/private/native-1.jsonl")
	for range 2 {
		queued, err := queueAdmissionIntent(home, "claude", hookEventStart, payload, at)
		if err != nil || !queued {
			t.Fatalf("queue = %t, %v", queued, err)
		}
	}
	if err := ReplayAdmissionIntents(home, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := ReplayAdmissionIntents(home, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	regs, err := state.OpenReadOnly(home).LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("registrations = %#v, %v", regs, err)
	}
	entries, err := os.ReadDir(admissionIntentDir(home))
	if err != nil || len(entries) != 0 {
		t.Fatalf("remaining intents = %#v, %v", entries, err)
	}

	queued, err := queueAdmissionIntent(home, "claude", hookEventStart, claudeStart(project, "native-expired", "startup", ""), at)
	if err != nil || !queued {
		t.Fatalf("queue expired = %t, %v", queued, err)
	}
	if err := ReplayAdmissionIntents(home, at.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	regs, err = state.OpenReadOnly(home).LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("expired intent registered: %#v, %v", regs, err)
	}
}

func TestAdmissionIntentDropsParentWhenNestedProjectIsConfigured(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	nested := filepath.Join(project, "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	queued, err := queueAdmissionIntent(home, "claude", hookEventStart, claudeStart(nested, "native-nested", "startup", ""), at)
	if err != nil || !queued {
		t.Fatalf("queue = %t, %v", queued, err)
	}
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{ProjectID: archive.ProjectID(nested), Root: nested, ActivatedAt: at.Add(-time.Hour), Included: false})
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if err := PruneAdmissionIntents(home, cfg.Archive.Projects); err != nil {
		t.Fatal(err)
	}
	if err := ReplayAdmissionIntents(home, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(admissionIntentDir(home))
	if err != nil || len(entries) != 0 {
		t.Fatalf("nested intent retained: %#v, %v", entries, err)
	}
	regs, err := state.OpenReadOnly(home).LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatalf("nested session admitted: %#v, %v", regs, err)
	}
}

func TestAdmissionIntentRechecksProjectAndQueueBound(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	payload := claudeStart(project, "native-1", "startup", "")
	queued, err := queueAdmissionIntent(home, "claude", hookEventStart, payload, at)
	if err != nil || !queued {
		t.Fatalf("queue = %t, %v", queued, err)
	}
	cfg, found, err := config.Load(home)
	if err != nil || !found {
		t.Fatalf("config = %v, %v", found, err)
	}
	cfg.Archive.Projects[0].Included = false
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if err := PruneAdmissionIntents(home, cfg.Archive.Projects); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(admissionIntentDir(home))
	if err != nil || len(entries) != 0 {
		t.Fatalf("excluded intent retained: %#v, %v", entries, err)
	}
	if err := ReplayAdmissionIntents(home, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	regs, err := state.OpenReadOnly(home).LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatalf("excluded project registered: %#v, %v", regs, err)
	}
	queued, err = queueAdmissionIntent(home, "claude", hookEventStart, payload, at)
	if err != nil || queued {
		t.Fatalf("excluded queue = %t, %v", queued, err)
	}

	cfg.Archive.Projects[0].Included = true
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(admissionIntentDir(home), 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range maxAdmissionIntents {
		if err := os.WriteFile(filepath.Join(admissionIntentDir(home), fmt.Sprintf("%03d.json", i)), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	queued, err = queueAdmissionIntent(home, "claude", hookEventStart, payload, at)
	if queued || err == nil || !strings.Contains(err.Error(), "full") {
		t.Fatalf("full queue = %t, %v", queued, err)
	}
}

func TestAdmissionIntentDoesNotRewindLaterRegistration(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	payload := claudeStart(project, "native-1", "startup", "")
	queued, err := queueAdmissionIntent(home, "claude", hookEventStart, payload, at)
	if err != nil || !queued {
		t.Fatalf("queue = %t, %v", queued, err)
	}
	later := at.Add(time.Minute)
	if err := HandleEvent(home, "claude", payload, later); err != nil {
		t.Fatal(err)
	}
	if err := ReplayAdmissionIntents(home, later.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	regs, err := state.OpenReadOnly(home).LoadRegistrations()
	if err != nil || len(regs) != 1 || !regs[0].RegisteredAt.Equal(later) {
		t.Fatalf("replay rewound registration: %#v, %v", regs, err)
	}
}
