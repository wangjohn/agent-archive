package capture

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/state"
)

// An admission intent contains the minimum identity needed to retry a proven
// start. It deliberately has no raw hook payload or conversation text. The
// original proof is recorded at hook time because a transcript may no longer
// be empty when the collector retries the start.
type admissionIntent struct {
	Harness         string    `json:"harness"`
	Event           string    `json:"event"`
	NativeSessionID string    `json:"native_session_id"`
	ProjectRoot     string    `json:"project_root"`
	TranscriptPath  string    `json:"transcript_path,omitempty"`
	CursorVersion   string    `json:"cursor_version,omitempty"`
	ComposerMode    string    `json:"composer_mode,omitempty"`
	ObservedAt      time.Time `json:"observed_at"`
}

const (
	maxAdmissionIntents   = 128
	maxAdmissionIntentAge = 24 * time.Hour
	admissionQueueWait    = 200 * time.Millisecond
)

func admissionIntentDir(home string) string { return filepath.Join(home, "admission-intents") }

// ClearAdmissionIntents discards starts observed before a pause. Callers hold
// hooks.lock while changing the pause flag, so a hook that times out during
// that change either queues before this purge or observes the paused config.
func ClearAdmissionIntents(home string) error {
	unlock, err := local.NamedLockWait(home, "admission-intents.lock", 2*time.Second)
	if err != nil {
		return fmt.Errorf("lock admission intent queue: %w", err)
	}
	defer unlock()
	entries, err := os.ReadDir(admissionIntentDir(home))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if err := os.Remove(filepath.Join(admissionIntentDir(home), entry.Name())); err != nil && !os.IsNotExist(err) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// queueAdmissionIntent is only used after hooks.lock times out. It never
// queues an excluded, paused, pre-activation, or unproven start. The queue
// lock bounds the directory count across concurrent hook processes.
func queueAdmissionIntent(home, harness string, kind hookEventKind, payload map[string]any, now time.Time) (bool, error) {
	if !startsCapture(kind, harness) || !provesFreshSessionStart(harness, payload) || setupjournal.TransactionPending(home) {
		return false, nil
	}
	cfg, found, err := config.Load(home)
	if err != nil || !found || !cfg.Archive.Enabled || cfg.Paused {
		return false, err
	}
	project, owned := ConfiguredProjectActivationFor(cfg, projectRoot(payload))
	if !owned || !project.Included || !cfg.Archive.Eligible(project.Root, now) {
		return false, nil
	}
	nativeID := firstNonEmptyString(payload, "session_id", "conversation_id")
	if nativeID == "" {
		return false, nil
	}
	intent := admissionIntent{
		Harness: archive.CanonicalHarness(harness), Event: firstNonEmptyString(payload, "hook_event_name"),
		NativeSessionID: nativeID, ProjectRoot: project.Root, ObservedAt: now.UTC(),
		CursorVersion: firstNonEmptyString(payload, "cursor_version"), ComposerMode: firstNonEmptyString(payload, "composer_mode"),
	}
	if intent.Harness == "cursor" {
		intent.TranscriptPath = cursorTranscriptPath(payload, nativeID)
	} else {
		intent.TranscriptPath = firstNonEmptyString(payload, "transcript_path")
	}
	unlock, err := local.NamedLockWait(home, "admission-intents.lock", admissionQueueWait)
	if err != nil {
		return false, fmt.Errorf("lock admission intent queue: %w", err)
	}
	defer unlock()
	entries, err := os.ReadDir(admissionIntentDir(home))
	if !os.IsNotExist(err) && err != nil {
		return false, err
	}
	if len(entries) >= maxAdmissionIntents {
		return false, errors.New("admission intent queue is full")
	}
	id, err := local.ID()
	if err != nil {
		return false, err
	}
	if err := local.Write(filepath.Join(admissionIntentDir(home), fmt.Sprintf("%020d-%s.json", now.UTC().UnixNano(), id)), intent); err != nil {
		return false, err
	}
	return true, nil
}

// ReplayAdmissionIntents runs before the collector reads registrations. The
// current configuration and project activation are checked again; an intent
// never revives an excluded project. Successfully handled and expired intents
// are removed, while transient failures remain for the next pass. Hook
// registration is idempotent by native session ID.
func ReplayAdmissionIntents(home string, now time.Time) error {
	if setupjournal.TransactionPending(home) {
		return nil
	}
	unlock, err := local.NamedLockWait(home, "hooks.lock", 2*time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	entries, err := os.ReadDir(admissionIntentDir(home))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	cfg, found, err := config.Load(home)
	if err != nil {
		return err
	}
	if !found || !cfg.Archive.Enabled || cfg.Paused {
		return nil
	}
	store, err := state.Open(home)
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(admissionIntentDir(home), entry.Name())
		var intent admissionIntent
		if err := local.Read(path, &intent); err != nil {
			failures = append(failures, fmt.Errorf("read admission intent: %w", err))
			continue
		}
		remove := now.Sub(intent.ObservedAt) > maxAdmissionIntentAge || intent.ObservedAt.After(now.Add(time.Minute))
		project, owned := ConfiguredProjectActivationFor(cfg, intent.ProjectRoot)
		if !owned || !project.Included || project.Root != intent.ProjectRoot || !cfg.Archive.Eligible(project.Root, intent.ObservedAt) {
			remove = true
		}
		if !remove {
			payload := map[string]any{"hook_event_name": intent.Event, "session_id": intent.NativeSessionID, "cwd": intent.ProjectRoot, "transcript_path": intent.TranscriptPath, "cursor_version": intent.CursorVersion, "composer_mode": intent.ComposerMode}
			if !startsCapture(classifyHookEvent(intent.Harness, intent.Event), intent.Harness) || intent.NativeSessionID == "" {
				remove = true
			} else {
				// A live hook, or an earlier intent for this session, may have
				// registered it since this record was queued. Replaying that old
				// start as a continuation would move RegisteredAt backwards and
				// write duplicate lifecycle evidence.
				registered, err := HasRegistration(store, intent.NativeSessionID)
				if err != nil {
					failures = append(failures, fmt.Errorf("look up admission intent: %w", err))
					continue
				}
				if !registered {
					if err := handleSessionStartWithProof(home, store, cfg, intent.Harness, intent.NativeSessionID, intent.Event, payload, intent.ObservedAt, true); err != nil {
						failures = append(failures, fmt.Errorf("replay admission intent: %w", err))
						continue
					}
				}
				remove = true
			}
		}
		if remove {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}
