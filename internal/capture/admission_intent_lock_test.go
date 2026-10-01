package capture

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// queueFiles lists the admission queue folder: queued intents and any staged
// temporary files. A missing folder lists as nil.
func queueFiles(t *testing.T, home string) []string {
	t.Helper()
	entries, err := os.ReadDir(admissionIntentDir(home))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// A queued intent's disk syncs happen outside the queue lock, so setup's
// prune and pause's clear find it free while a hook is syncing. With the
// syncs under the lock, both waited for them, and on a busy machine one sync
// outlasted their 2s waits.
//
// Regression: 2026-09-30, measured holds of admission-intents.lock reached
// 3.75s under load and setup's prune gave up at 2s.
func TestPruneAndClearDoNotWaitForAHookSyncingItsAdmissionIntent(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	if err := os.MkdirAll(admissionIntentDir(home), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, found, err := config.Load(home)
	if err != nil || !found {
		t.Fatalf("config = %v, %v", found, err)
	}
	var pruneErr, clearErr error
	queued, err := queueAdmissionIntentAfterStage(home, "claude", hookEventStart, claudeStart(project, "native-1", "startup", ""), at, func() {
		// Neither waits: the lock is free while the hook's file is synced.
		release, lockErr := local.NamedLock(home, "admission-intents.lock")
		if lockErr != nil {
			pruneErr = fmt.Errorf("the queue lock is held while the hook syncs: %w", lockErr)
			return
		}
		release()
		pruneErr = PruneAdmissionIntents(home, cfg)
		clearErr = ClearAdmissionIntents(home)
	})
	if pruneErr != nil || clearErr != nil {
		t.Fatalf("prune = %v, clear = %v", pruneErr, clearErr)
	}
	if err != nil || !queued {
		t.Fatalf("queue = %t, %v", queued, err)
	}
	if files := queueFiles(t, home); len(files) != 1 || !strings.HasSuffix(files[0], ".json") {
		t.Fatalf("queue folder = %v, want the one intent", files)
	}
}

// A hook that staged its intent before setup excluded the project and
// pruned rechecks under the queue lock: it queues nothing and removes the
// file it staged.
func TestAHookStagingDuringSetupsPruneQueuesNothingForAnExcludedProject(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	if err := os.MkdirAll(admissionIntentDir(home), 0o700); err != nil {
		t.Fatal(err)
	}
	var setupErr error
	queued, err := queueAdmissionIntentAfterStage(home, "claude", hookEventStart, claudeStart(project, "native-1", "startup", ""), at, func() {
		cfg, _, loadErr := config.Load(home)
		if loadErr != nil {
			setupErr = loadErr
			return
		}
		cfg.Archive.Projects[0].Included = false
		if setupErr = config.Save(home, cfg); setupErr == nil {
			setupErr = PruneAdmissionIntents(home, cfg)
		}
	})
	if setupErr != nil {
		t.Fatal(setupErr)
	}
	if err != nil || queued {
		t.Fatalf("queue = %t, %v; want nothing queued for an excluded project", queued, err)
	}
	if files := queueFiles(t, home); len(files) != 0 {
		t.Fatalf("queue folder = %v, want empty", files)
	}
}

// Uninstall's purge deletes the queue folder holding the queue lock. A hook
// that overlaps it never leaves the folder behind: it stages outside the
// lock only into a folder that already exists, and finds no configuration
// under the lock.
func TestAHookOverlappingAPurgeLeavesNoIntentFolder(t *testing.T) {
	t.Parallel()
	for _, folderExisted := range []bool{true, false} {
		t.Run(fmt.Sprintf("folder existed %t", folderExisted), func(t *testing.T) {
			t.Parallel()
			home, project := t.TempDir(), t.TempDir()
			at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
			setUpTestConfig(t, home, project, at.Add(-time.Hour))
			if folderExisted {
				if err := os.MkdirAll(admissionIntentDir(home), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			var purgeErr error
			queued, err := queueAdmissionIntentAfterStage(home, "claude", hookEventStart, claudeStart(project, "native-1", "startup", ""), at, func() {
				// A purge may already have run: staging outside the lock
				// must not have created the folder.
				if _, statErr := os.Stat(admissionIntentDir(home)); !folderExisted && !os.IsNotExist(statErr) {
					purgeErr = errors.New("staging outside the lock created the queue folder")
					return
				}
				release, lockErr := local.NamedLock(home, "admission-intents.lock")
				if lockErr != nil {
					purgeErr = lockErr
					return
				}
				defer release()
				purgeErr = os.RemoveAll(admissionIntentDir(home))
				if purgeErr == nil {
					purgeErr = os.Remove(filepath.Join(home, "config.json"))
				}
			})
			if purgeErr != nil {
				t.Fatal(purgeErr)
			}
			if err != nil || queued {
				t.Fatalf("queue = %t, %v; want nothing queued after a purge", queued, err)
			}
			if _, err := os.Stat(admissionIntentDir(home)); !os.IsNotExist(err) {
				t.Fatalf("the purged queue folder is back: %v (%v)", err, queueFiles(t, home))
			}
		})
	}
}

// Hooks stage their intents in the queue folder before taking the lock.
// Those temporary files, the counting hook's own among them, do not take
// queue places.
func TestStagedIntentFilesDoNotFillTheAdmissionQueue(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	if err := os.MkdirAll(admissionIntentDir(home), 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range maxAdmissionIntents - 1 {
		if err := os.WriteFile(filepath.Join(admissionIntentDir(home), fmt.Sprintf("%03d.json", i)), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 3 { // other hooks' staged files
		if err := os.WriteFile(filepath.Join(admissionIntentDir(home), fmt.Sprintf(".pending-%d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	queued, err := queueAdmissionIntent(home, "claude", hookEventStart, claudeStart(project, "native-last", "startup", ""), at)
	if err != nil || !queued {
		t.Fatalf("last place = %t, %v", queued, err)
	}
	queued, err = queueAdmissionIntent(home, "claude", hookEventStart, claudeStart(project, "native-over", "startup", ""), at)
	if queued || err == nil || !strings.Contains(err.Error(), "full") {
		t.Fatalf("over the bound = %t, %v", queued, err)
	}
}

// A rename under the queue lock can still stall for over a second on a busy
// machine. Setup's prune outwaits a holder stalled past the 2s it used to wait.
func TestPruneOutwaitsAQueueHolderStalledForSeconds(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	queued, err := queueAdmissionIntent(home, "claude", hookEventStart, claudeStart(project, "native-1", "startup", ""), at)
	if err != nil || !queued {
		t.Fatalf("queue = %t, %v", queued, err)
	}
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Archive.Projects[0].Included = false
	release, err := local.NamedLock(home, "admission-intents.lock")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(3 * time.Second)
		release()
	}()
	if err := PruneAdmissionIntents(home, cfg); err != nil {
		t.Fatalf("prune gave up on a holder stalled for 3s: %v", err)
	}
	if files := queueFiles(t, home); len(files) != 0 {
		t.Fatalf("queue folder = %v, want the excluded project's intent pruned", files)
	}
}
