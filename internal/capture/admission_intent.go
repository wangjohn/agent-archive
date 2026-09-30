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
// start or a Cursor transcript-path follow-up. It deliberately has no raw hook
// payload or conversation text. The original start proof is recorded at hook
// time because a transcript may no longer be empty when the collector retries.
type admissionIntent struct {
	Harness         string    `json:"harness"`
	Event           string    `json:"event"`
	NativeSessionID string    `json:"native_session_id"`
	ProjectRoot     string    `json:"project_root"`
	DestinationID   string    `json:"destination_id"`
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

type cursorFollowupEvent string

const (
	cursorResponseEvent cursorFollowupEvent = "afterAgentResponse"
	cursorStopEvent     cursorFollowupEvent = "stop"
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

// PruneAdmissionIntents removes retry records whose project or destination
// setup changed. The queue lock also serializes this with hook-side writes.
func PruneAdmissionIntents(home string, cfg config.Config) error {
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
		path := filepath.Join(admissionIntentDir(home), entry.Name())
		var intent admissionIntent
		readErr := local.Read(path, &intent)
		if readErr == nil && intent.DestinationID == cfg.DestinationID() && intentProjectStillOwned(intent.ProjectRoot, cfg.Archive.Projects) {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// Without retaining the hook's original cwd, an intent for a parent project
// cannot be distinguished from one that started in a newly configured nested
// project. Drop those ambiguous intents rather than admitting them under the
// parent after setup changes ownership.
func intentProjectStillOwned(root string, projects []archive.ProjectActivation) bool {
	included := false
	for _, project := range projects {
		if project.Root == root {
			included = project.Included
			continue
		}
		if local.PathWithin(resolvedPath(project.Root), resolvedPath(root)) {
			return false
		}
	}
	return included
}

// queueAdmissionIntent is only used after hooks.lock times out. It queues a
// proven start, or a Cursor response/stop that supplies a valid transcript
// path. A follow-up can update an existing registration but never admit a new
// session. The queue lock bounds the count across concurrent hook processes.
func queueAdmissionIntent(home, harness string, kind hookEventKind, payload map[string]any, now time.Time) (bool, error) {
	intent, queued, err := hookAdmissionIntent(home, harness, kind, payload, now)
	if err != nil || !queued {
		return false, err
	}
	unlock, err := local.NamedLockWait(home, "admission-intents.lock", admissionQueueWait)
	if err != nil {
		return false, fmt.Errorf("lock admission intent queue: %w", err)
	}
	defer unlock()
	return writeAdmissionIntent(home, intent, payload)
}

func hookAdmissionIntent(home, harness string, kind hookEventKind, payload map[string]any, now time.Time) (admissionIntent, bool, error) {
	if setupjournal.TransactionPending(home) {
		return admissionIntent{}, false, nil
	}
	nativeID := firstNonEmptyString(payload, "session_id", "conversation_id")
	cursorPath := cursorTranscriptPath(payload, nativeID)
	start := startsCapture(kind, harness) && provesFreshSessionStart(harness, payload)
	followup := archive.CanonicalHarness(harness) == "cursor" &&
		(kind == hookEventResponse || cursorFollowupEvent(firstNonEmptyString(payload, "hook_event_name")) == cursorStopEvent) && cursorPath != ""
	if !start && !followup {
		return admissionIntent{}, false, nil
	}
	cfg, found, err := config.Load(home)
	if err != nil || !found || !cfg.Archive.Enabled || cfg.Paused {
		return admissionIntent{}, false, err
	}
	project, owned := ConfiguredProjectActivationFor(cfg, projectRoot(payload))
	if !owned || !project.Included || !cfg.Archive.Eligible(project.Root, now) {
		return admissionIntent{}, false, nil
	}
	if nativeID == "" {
		return admissionIntent{}, false, nil
	}
	intent := admissionIntent{
		Harness: archive.CanonicalHarness(harness), Event: firstNonEmptyString(payload, "hook_event_name"),
		NativeSessionID: nativeID, ProjectRoot: project.Root, DestinationID: cfg.DestinationID(), ObservedAt: now.UTC(),
		CursorVersion: firstNonEmptyString(payload, "cursor_version"), ComposerMode: firstNonEmptyString(payload, "composer_mode"),
	}
	if intent.Harness == "cursor" {
		intent.TranscriptPath = cursorPath
	} else {
		intent.TranscriptPath = firstNonEmptyString(payload, "transcript_path")
	}
	return intent, true, nil
}

func writeAdmissionIntent(home string, intent admissionIntent, payload map[string]any) (bool, error) {
	// A pause or setup may have committed while this hook waited for the
	// queue lock. Recheck after taking it so a stale config cannot write a
	// private retry record after the corresponding purge.
	cfg, found, err := config.Load(home)
	if err != nil || !found || !cfg.Archive.Enabled || cfg.Paused || setupjournal.TransactionPending(home) {
		return false, err
	}
	current, owned := ConfiguredProjectActivationFor(cfg, projectRoot(payload))
	if !owned || !current.Included || current.Root != intent.ProjectRoot || cfg.DestinationID() != intent.DestinationID || !cfg.Archive.Eligible(current.Root, intent.ObservedAt) {
		return false, nil
	}
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
	if err := local.Write(filepath.Join(admissionIntentDir(home), fmt.Sprintf("%020d-%s.json", intent.ObservedAt.UnixNano(), id)), intent); err != nil {
		return false, err
	}
	return true, nil
}

type deferredFollowup struct {
	path   string
	intent admissionIntent
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
	var deferred []deferredFollowup
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(admissionIntentDir(home), entry.Name())
		followup, err := replayAdmissionFile(home, store, cfg, path, now)
		if err != nil {
			failures = append(failures, err)
		}
		if followup != nil {
			deferred = append(deferred, *followup)
		}
	}
	for _, followup := range deferred {
		if err := replayDeferredFollowup(store, cfg, followup); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func replayAdmissionFile(home string, store *state.Store, cfg config.Config, path string, now time.Time) (*deferredFollowup, error) {
	var intent admissionIntent
	if err := local.Read(path, &intent); err != nil {
		return nil, fmt.Errorf("read admission intent: %w", err)
	}
	if !replayIntentEligible(cfg, intent, now) {
		return nil, removeAdmissionIntent(path)
	}
	payload := intentPayload(intent)
	start, followup := replayIntentKinds(intent, payload)
	if (!start && !followup) || intent.NativeSessionID == "" {
		return nil, removeAdmissionIntent(path)
	}
	registered, err := HasRegistration(store, intent.NativeSessionID)
	if err != nil {
		return nil, fmt.Errorf("look up admission intent: %w", err)
	}
	if registered {
		matches, err := replayRegistrationMatches(store, cfg, intent)
		if err != nil {
			return nil, fmt.Errorf("validate admission intent registration: %w", err)
		}
		if !matches {
			return nil, removeAdmissionIntent(path)
		}
	}
	if !registered && followup && !start {
		return &deferredFollowup{path: path, intent: intent}, nil
	}
	if err := replayAdmissionAction(home, store, cfg, intent, payload, registered, start, followup); err != nil {
		return nil, err
	}
	return nil, removeAdmissionIntent(path)
}

func replayIntentEligible(cfg config.Config, intent admissionIntent, now time.Time) bool {
	if now.Sub(intent.ObservedAt) > maxAdmissionIntentAge || intent.ObservedAt.After(now.Add(time.Minute)) {
		return false
	}
	project, owned := ConfiguredProjectActivationFor(cfg, intent.ProjectRoot)
	return owned && project.Included && project.Root == intent.ProjectRoot &&
		intent.DestinationID == cfg.DestinationID() &&
		intentProjectStillOwned(intent.ProjectRoot, cfg.Archive.Projects) &&
		cfg.Archive.Eligible(project.Root, intent.ObservedAt)
}

func intentPayload(intent admissionIntent) map[string]any {
	return map[string]any{
		"hook_event_name": intent.Event, "session_id": intent.NativeSessionID,
		"cwd": intent.ProjectRoot, "transcript_path": intent.TranscriptPath,
		"cursor_version": intent.CursorVersion, "composer_mode": intent.ComposerMode,
	}
}

func replayIntentKinds(intent admissionIntent, payload map[string]any) (start, followup bool) {
	start = startsCapture(classifyHookEvent(intent.Harness, intent.Event), intent.Harness)
	followup = intent.Harness == "cursor" &&
		(cursorFollowupEvent(intent.Event) == cursorResponseEvent || cursorFollowupEvent(intent.Event) == cursorStopEvent) &&
		cursorTranscriptPath(payload, intent.NativeSessionID) != ""
	return start, followup
}

func replayAdmissionAction(home string, store *state.Store, cfg config.Config, intent admissionIntent, payload map[string]any, registered, start, followup bool) error {
	switch {
	case !registered && start:
		if err := handleSessionStartWithProof(home, store, cfg, intent.Harness, intent.NativeSessionID, intent.Event, payload, intent.ObservedAt, true, ""); err != nil {
			return fmt.Errorf("replay admission intent: %w", err)
		}
	case registered && followup:
		if err := handleSessionStop(store, intent.Harness, intent.NativeSessionID, intent.Event, payload, intent.ObservedAt); err != nil {
			return fmt.Errorf("replay Cursor follow-up: %w", err)
		}
	case registered && start && intent.Harness == "cursor" && intent.TranscriptPath != "":
		return adoptQueuedCursorPath(store, intent, payload)
	}
	return nil
}

func adoptQueuedCursorPath(store *state.Store, intent admissionIntent, payload map[string]any) error {
	archiveID, _, err := store.ArchiveSessionID(intent.NativeSessionID)
	if err != nil {
		return err
	}
	reg, found, err := store.LoadRegistration(archiveID)
	if err != nil {
		return fmt.Errorf("load registered Cursor start: %w", err)
	}
	if !found {
		return errors.New("registered Cursor start disappeared")
	}
	if err := adoptCursorTranscriptPath(store, &reg, intent.Harness, payload); err != nil {
		return fmt.Errorf("adopt queued Cursor path: %w", err)
	}
	return nil
}

func replayDeferredFollowup(store *state.Store, cfg config.Config, followup deferredFollowup) error {
	registered, err := HasRegistration(store, followup.intent.NativeSessionID)
	if err != nil {
		return fmt.Errorf("look up deferred Cursor follow-up: %w", err)
	}
	if !registered {
		// A follow-up never admits a session. Keep it until a start or expiry.
		return nil
	}
	matches, err := replayRegistrationMatches(store, cfg, followup.intent)
	if err != nil {
		return fmt.Errorf("validate deferred Cursor follow-up: %w", err)
	}
	if !matches {
		return removeAdmissionIntent(followup.path)
	}
	if err := handleSessionStop(store, "cursor", followup.intent.NativeSessionID, followup.intent.Event, intentPayload(followup.intent), followup.intent.ObservedAt); err != nil {
		return fmt.Errorf("replay deferred Cursor follow-up: %w", err)
	}
	return removeAdmissionIntent(followup.path)
}

func removeAdmissionIntent(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func replayRegistrationMatches(store *state.Store, cfg config.Config, intent admissionIntent) (bool, error) {
	archiveID, found, err := store.ArchiveSessionID(intent.NativeSessionID)
	if err != nil || !found {
		return false, err
	}
	reg, found, err := store.LoadRegistration(archiveID)
	if err != nil || !found {
		return false, err
	}
	return filepath.Clean(reg.ProjectRoot) == filepath.Clean(intent.ProjectRoot) &&
		archive.CanonicalHarness(reg.Harness.Name) == intent.Harness && cfg.AcceptSession(reg), nil
}
