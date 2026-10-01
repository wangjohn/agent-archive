package capture

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestStagedAdmissionDoesNotCrossPauseResume(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	if err := os.MkdirAll(admissionIntentDir(home), 0700); err != nil {
		t.Fatal(err)
	}
	queued, err := queueAdmissionIntentAfterStage(home, "claude", hookEventStart, claudeStart(project, "native", "startup", ""), at, func() {
		if _, err := config.SetPaused(home, true); err != nil {
			t.Fatal(err)
		}
		if err := ClearAdmissionIntents(home); err != nil {
			t.Fatal(err)
		}
		if _, err := config.SetPaused(home, false); err != nil {
			t.Fatal(err)
		}
		if err := ClearAdmissionIntents(home); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ReplayAdmissionIntents(home, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	regs, err := state.OpenReadOnly(home).LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	if queued || len(regs) != 0 {
		t.Fatalf("staged admission survived pause/resume: queued=%t registrations=%d", queued, len(regs))
	}
}

// Simulate a crash leaving an intent after the pause flag was durably saved
// but before the queue purge. Replay must reject the old capture window even
// when the archive has since resumed. Also check the first legacy transition.
func TestAdmissionReplayDoesNotCrossPauseResume(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
			setUpTestConfig(t, home, project, at.Add(-time.Hour))
			if !legacy {
				if _, err := config.SetPaused(home, true); err != nil {
					t.Fatal(err)
				}
				if _, err := config.SetPaused(home, false); err != nil {
					t.Fatal(err)
				}
			}
			queued, err := queueAdmissionIntent(home, "claude", hookEventStart, claudeStart(project, "old-native", "startup", ""), at)
			if err != nil || !queued {
				t.Fatalf("queue=%t error=%v", queued, err)
			}
			if _, err := config.SetPaused(home, true); err != nil {
				t.Fatal(err)
			}
			if _, err := config.SetPaused(home, false); err != nil {
				t.Fatal(err)
			}
			if err := ReplayAdmissionIntents(home, at.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			regs, err := state.OpenReadOnly(home).LoadRegistrations()
			if err != nil || len(regs) != 0 {
				t.Fatalf("stale replay registrations=%v error=%v", regs, err)
			}
			if files := queueFiles(t, home); len(files) != 0 {
				t.Fatalf("stale queue=%v", files)
			}
			queued, err = queueAdmissionIntent(home, "claude", hookEventStart, claudeStart(project, "new-native", "startup", ""), at.Add(2*time.Second))
			if err != nil || !queued {
				t.Fatalf("fresh queue=%t error=%v", queued, err)
			}
			if err := ReplayAdmissionIntents(home, at.Add(3*time.Second)); err != nil {
				t.Fatal(err)
			}
			regs, err = state.OpenReadOnly(home).LoadRegistrations()
			if err != nil || len(regs) != 1 || regs[0].NativeSessionID != "new-native" {
				t.Fatalf("fresh replay registrations=%v error=%v", regs, err)
			}
		})
	}
}

// The lock seam completes pause and resume while a hook is waiting. Both
// getting the lock and timing out must decline the event's original window.
func TestHookAdmissionWaitDoesNotCrossPauseResume(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(fmt.Sprintf("busy=%t", busy), func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
			setUpTestConfig(t, home, project, at.Add(-time.Hour))
			lock := func(_ string, _ time.Duration) (func(), error) {
				if _, err := config.SetPaused(home, true); err != nil {
					t.Fatal(err)
				}
				if err := ClearAdmissionIntents(home); err != nil {
					t.Fatal(err)
				}
				if _, err := config.SetPaused(home, false); err != nil {
					t.Fatal(err)
				}
				if err := ClearAdmissionIntents(home); err != nil {
					t.Fatal(err)
				}
				if busy {
					return nil, local.ErrBusy
				}
				return func() {}, nil
			}
			err := handleEvent(home, "claude", claudeStart(project, "old-native", "startup", ""), at, lock, nil, nil)
			if err != nil && !errors.Is(err, local.ErrBusy) {
				t.Fatal(err)
			}
			if err := ReplayAdmissionIntents(home, at.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			regs, err := state.OpenReadOnly(home).LoadRegistrations()
			if err != nil || len(regs) != 0 {
				t.Fatalf("cross-window admission=%v error=%v", regs, err)
			}
			if files := queueFiles(t, home); len(files) != 0 {
				t.Fatalf("stale queue=%v", files)
			}
		})
	}
}

func TestFreshHookDefersAndReplaysDuringIdentityMigration(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an upgrade with a backlog of old registrations and missing derived indexes.
	for i := 0; i < 120; i++ {
		r := archive.SessionRegistration{ArchiveSessionID: fmt.Sprintf("legacy-%03d", i), NativeSessionID: fmt.Sprintf("old-%03d", i), Harness: archive.Harness{Name: "claude"}, ProjectRoot: project, ProjectID: archive.ProjectID(project), SessionStartedAt: at.Add(-time.Minute)}
		if err := store.SaveRegistration(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := HandleEvent(home, "claude", claudeStart(project, "fresh", "startup", ""), at); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.AgentSessionID("claude", "fresh"); err != nil || found {
		t.Fatal("fresh start allocated before migration", err)
	}
	entries, err := os.ReadDir(admissionIntentDir(home))
	if err != nil || len(entries) != 1 {
		t.Fatal("fresh proof not durably deferred", err)
	}
	for n := 0; n < 20; n++ {
		done, err := PrepareIdentityIndexes(home, 256)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	if err := ReplayAdmissionIntents(home, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	id, found, err := store.AgentSessionID("claude", "fresh")
	if err != nil || !found {
		t.Fatal("deferred hook not admitted", err)
	}
	r, _, _ := store.LoadRegistration(id)
	if !r.SessionStartedAt.Equal(at) {
		t.Fatal("replay substituted migration time for start")
	}
	cfg, _, err := config.Load(home)
	if err != nil || cfg.Discovery == nil || cfg.Discovery.Enabled {
		t.Fatal("upgrade accidentally enabled discovery", err)
	}
}
