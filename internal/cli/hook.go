package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// runHookCommand implements the hidden `_hook` entry point hooks.Merge
// installs into each harness's own hook configuration. A hook has a short
// timeout (2s, per hooks.Merge) and must never block the user's turn, so
// this always exits 0; a problem is reported to stderr only, matching the
// spec's failure table ("Hook cannot write a request: task continues,
// diagnostic is available outside model context").
func runHookCommand(args []string, stdin io.Reader, stderr io.Writer, env Env) int {
	fs := flag.NewFlagSet("_hook", flag.ContinueOnError)
	fs.SetOutput(stderr)
	harness := fs.String("harness", "", "harness name (codex, claude, cursor)")
	if err := fs.Parse(args); err != nil {
		return 0
	}
	var payload map[string]any
	// A hook that sends no or malformed JSON is treated as a no-op, not an
	// error: some hook events (per the harness's own docs) carry no useful
	// fields at all, and we must never fail loudly on the harness's input.
	_ = json.NewDecoder(stdin).Decode(&payload)

	home, err := env.home()
	if err != nil {
		terminal.Printf(stderr, "agent-archive: hook: resolve home: %v\n", err)
		return 0
	}
	if err := handleHookEvent(home, *harness, payload, env.now()); err != nil {
		terminal.Printf(stderr, "agent-archive: hook: %v\n", err)
	}
	return 0
}

type hookEventKind int

const (
	hookEventIgnored hookEventKind = iota
	hookEventStart
	hookEventTurnStart
	hookEventStop
	hookEventSubagentStop
	hookEventResponse
)

// classifyHookEvent mirrors, per harness, exactly the event names
// hooks.Merge installs (see internal/hooks/hooks.go's events lists) and the
// spec's Lifecycle integration table. Anything else installed alongside
// these (PreToolUse, PostToolUse, ...) is not an
// archive-relevant event and is ignored here.
func classifyHookEvent(harness, eventName string) hookEventKind {
	switch strings.ToLower(strings.TrimSpace(harness)) {
	case "codex":
		//lint:ignore LV1001 eventName is the hook_event_name a harness sends; any other name is expected and ignored
		switch eventName {
		case "SessionStart":
			return hookEventStart
		case "UserPromptSubmit":
			return hookEventTurnStart
		case "Stop", "Interrupt", "SessionEnd":
			return hookEventStop
		case "SubagentStop":
			return hookEventSubagentStop
		}
	case "claude", "claude-code":
		//lint:ignore LV1001 eventName is the hook_event_name a harness sends; any other name is expected and ignored
		switch eventName {
		case "SessionStart":
			return hookEventStart
		case "UserPromptSubmit":
			return hookEventTurnStart
		case "Stop", "StopFailure", "SessionEnd":
			return hookEventStop
		case "SubagentStop":
			return hookEventSubagentStop
		}
	case "cursor":
		//lint:ignore LV1001 eventName is the hook_event_name a harness sends; any other name is expected and ignored
		switch eventName {
		case "sessionStart":
			return hookEventStart
		case "beforeSubmitPrompt":
			return hookEventTurnStart
		case "afterAgentResponse":
			return hookEventResponse
		case "stop", "sessionEnd":
			return hookEventStop
		case "subagentStop":
			return hookEventSubagentStop
		}
	}
	return hookEventIgnored
}

func handleHookEvent(home, harness string, payload map[string]any, now time.Time) error {
	if payload == nil {
		return nil
	}
	eventName, _ := payload["hook_event_name"].(string)
	kind := classifyHookEvent(harness, eventName)
	if kind == hookEventIgnored {
		return nil
	}
	if transactionPending(home) {
		return recordSetupInProgress(home, kind, harness, payload, now)
	}
	unlock, lockErr := local.NamedLockWait(home, "hooks.lock", time.Second)
	if lockErr != nil {
		return fmt.Errorf("capture registration busy; this hook was not recorded: %w", lockErr)
	}
	defer unlock()
	// Setup may have started while this hook was waiting for the lock.
	if transactionPending(home) {
		return recordSetupInProgress(home, kind, harness, payload, now)
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if !found || !cfg.Archive.Enabled || cfg.Paused {
		return nil
	}
	store, err := collector.NewLocalStore(home)
	if err != nil {
		return fmt.Errorf("open local store: %w", err)
	}
	nativeSessionID := firstNonEmptyString(payload, "session_id", "conversation_id")
	if nativeSessionID == "" {
		return fmt.Errorf("hook payload for %s has no session identifier", eventName)
	}

	switch kind {
	case hookEventStart:
		err = handleSessionStart(home, store, cfg, harness, nativeSessionID, eventName, payload, now)
	case hookEventTurnStart:
		registered := true
		if canonicalHarness(harness) == "cursor" {
			registered, err = hasRegistration(store, nativeSessionID)
			if err != nil {
				return err
			}
		}
		if !registered {
			// Cursor's desktop app fires no sessionStart for a new chat
			// (observed on 3.21.13): its first hook is beforeSubmitPrompt.
			// A never-seen conversation is registered there, under the
			// same fresh-start proof a sessionStart would need.
			err = handleSessionStart(home, store, cfg, harness, nativeSessionID, eventName, payload, now)
		} else {
			err = handleSessionActivity(store, harness, nativeSessionID, eventName, payload, now)
		}
	case hookEventSubagentStop:
		err = handleSubagentStop(store, cfg, harness, nativeSessionID, payload, now)
	case hookEventStop, hookEventResponse:
		err = handleSessionStop(store, harness, nativeSessionID, eventName, payload, now)
	case hookEventIgnored:
		// Returned before the lock was taken.
	}
	// Retention can forget a session between this hook's registration lookup
	// and its request write; the store then refuses the write so no orphan
	// request is left. That is the intended outcome of the race, not a fault
	// to report on the user's turn.
	if errors.Is(err, collector.ErrSessionNotRegistered) {
		return nil
	}
	return err
}

// startsCapture reports whether an event is one that can register a new
// session: a start, or a Cursor first prompt (Cursor's app fires no start).
func startsCapture(kind hookEventKind, harness string) bool {
	return kind == hookEventStart || (kind == hookEventTurnStart && canonicalHarness(harness) == "cursor")
}

// recordSetupInProgress explains a session start that setup's own transaction
// window swallowed. Without it an included project simply never registers the
// session and `status` offers no reason, unlike the pre-activation and
// unknown-start cases. Setup holds hooks.lock while it commits, so this path
// never waits for it. The write takes only diagnostics.lock, for at most
// hookDiagnosticsWait, and drops the diagnostic on timeout: losing one
// bounded, content-free diagnostic is better than holding up the user's turn
// behind an installation. recordCaptureDiagnostic rechecks inclusion under
// that lock, so a project setup has just excluded and pruned stays pruned.
func recordSetupInProgress(home string, kind hookEventKind, harness string, payload map[string]any, now time.Time) error {
	if !startsCapture(kind, harness) {
		return nil
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if !found || !cfg.Archive.Enabled || cfg.Paused {
		return nil
	}
	if kind == hookEventTurnStart {
		// A Cursor prompt starts capture only for a never-seen chat. A
		// registered chat's prompt is a continuation, which setup's window
		// drops like any other activity; it is not a start to explain.
		nativeSessionID := firstNonEmptyString(payload, "session_id", "conversation_id")
		if nativeSessionID == "" {
			return nil
		}
		registered, err := hasRegistration(collector.OpenLocalStoreReadOnly(home), nativeSessionID)
		if err != nil || registered {
			return err
		}
	}
	// Same rule as every other diagnostic: an excluded project, or a directory
	// belonging to no configured project, never leaves its path on disk.
	project, owned := configuredProjectActivationFor(cfg, projectRoot(payload))
	if !owned || !project.Included {
		return nil
	}
	return recordCaptureDiagnostic(home, captureDiagnostic{
		Code: diagnosticSetupInProgress, Harness: canonicalHarness(harness),
		ProjectRoot: project.Root, ObservedAt: now,
	})
}

func handleSessionActivity(store *collector.LocalStore, harness, nativeSessionID, eventName string, payload map[string]any, now time.Time) error {
	archiveID, found, err := store.ArchiveSessionID(nativeSessionID)
	if err != nil {
		return fmt.Errorf("look up archive session ID: %w", err)
	}
	if !found {
		return nil
	}
	reg, found, err := store.LoadRegistration(archiveID)
	if err != nil {
		return err
	}
	if !found || canonicalHarness(reg.Harness.Name) != canonicalHarness(harness) {
		return nil
	}
	if eventName == "beforeSubmitPrompt" {
		if err := adoptCursorTranscriptPath(store, &reg, harness, payload); err != nil {
			return err
		}
	}
	return saveLifecycleEvidence(store, archiveID, harness, strings.ToLower(eventName), payload, now)
}

// hasRegistration reports whether a native session already has an accepted
// registration. An index entry without a registration does not count.
func hasRegistration(store *collector.LocalStore, nativeSessionID string) (bool, error) {
	archiveID, found, err := store.ArchiveSessionID(nativeSessionID)
	if err != nil {
		return false, fmt.Errorf("look up archive session ID: %w", err)
	}
	if !found {
		return false, nil
	}
	_, found, err = store.LoadRegistration(archiveID)
	return found, err
}

// cursorTranscriptPath returns the transcript path a Cursor payload names for
// this conversation, or "" when it names none the archive may read. Cursor
// writes a chat's transcript to .../agent-transcripts/<id>/<id>.jsonl, so the
// path must be absolute and its file name must be the conversation's own id;
// anything else is not provably this conversation's transcript.
func cursorTranscriptPath(payload map[string]any, conversationID string) string {
	path := firstNonEmptyString(payload, "transcript_path")
	if path == "" || conversationID == "" || !filepath.IsAbs(path) {
		return ""
	}
	path = filepath.Clean(path)
	if filepath.Base(path) != conversationID+".jsonl" {
		return ""
	}
	return path
}

// adoptCursorTranscriptPath fills in a Cursor registration's transcript path
// from a later event. A new desktop chat is registered at its first prompt,
// when Cursor has not yet named the transcript (transcript_path is null);
// afterAgentResponse and stop then carry it. A path already set is never
// replaced, whatever a later payload says. The write goes through
// UpdateRegistration, under the lock retention forgets a session with, so a
// chat forgotten meanwhile is not written back without its index entry; that
// is reported as collector.ErrSessionNotRegistered, which handleHookEvent
// treats as the quiet outcome of the race.
func adoptCursorTranscriptPath(store *collector.LocalStore, reg *archive.SessionRegistration, harness string, payload map[string]any) error {
	// A chat read from Cursor's database never switches to a file.
	if canonicalHarness(harness) != "cursor" || reg.TranscriptPath != "" || !reg.ReadsTranscriptFile() {
		return nil
	}
	path := cursorTranscriptPath(payload, reg.NativeSessionID)
	if path == "" {
		return nil
	}
	found, err := store.UpdateRegistration(reg.ArchiveSessionID, func(current *archive.SessionRegistration) error {
		if current.TranscriptPath == "" && current.ReadsTranscriptFile() {
			current.TranscriptPath = path
		}
		*reg = *current
		return nil
	})
	if err != nil {
		return fmt.Errorf("record transcript path: %w", err)
	}
	if !found {
		return collector.ErrSessionNotRegistered
	}
	return nil
}

func handleSessionStart(home string, store *collector.LocalStore, cfg config.Config, harness, nativeSessionID, eventName string, payload map[string]any, now time.Time) error {
	reason := strings.ToLower(eventName)
	transcriptPath, _ := payload["transcript_path"].(string)
	isCursor := canonicalHarness(harness) == "cursor"
	if isCursor {
		transcriptPath = cursorTranscriptPath(payload, nativeSessionID)
	}
	// A hook reports the session's working directory, which is only sometimes
	// the configured project root: a Claude Code worktree lives in
	// <project>/.claude/worktrees/<name>, and a session started from any
	// subdirectory reports that subdirectory. Resolve the configured project
	// that owns it and register under the configured spelling, so the project
	// ID, the activation boundary, and later continuations all agree with the
	// configuration rather than with the directory the user happened to be in.
	owner, owned := configuredProjectActivationFor(cfg, projectRoot(payload))
	root := projectRoot(payload)
	if owned {
		root = owner.Root
	}

	existingID, found, err := store.ArchiveSessionID(nativeSessionID)
	if err != nil {
		return fmt.Errorf("look up archive session ID: %w", err)
	}
	if found {
		// A continuation of a session we already registered: keep its
		// original start time, admission, and origin (an imported session a
		// hook resumes stays an import), and just refresh what may have
		// changed. The load, the checks, and the save all happen under the
		// lock retention forgets a session with, so a resume at the moment of
		// expiry cannot write the registration back after retention removed
		// it together with its index entry.
		updated, err := store.UpdateRegistration(existingID, func(existing *archive.SessionRegistration) error {
			if !cfg.AcceptSession(*existing) {
				return errContinuationDeclined
			}
			// Claude Code's hook cwd follows the session's working
			// directory (a persisted `cd`), so a continuation may report a
			// subdirectory of the project it started in. Match on project
			// identity: only a different harness or a different configured
			// project is a conflict.
			if canonicalHarness(existing.Harness.Name) != canonicalHarness(harness) {
				return errSessionIdentityConflict
			}
			if configured, ok := configuredProjectFor(cfg, root); ok && filepath.Clean(configured) != filepath.Clean(existing.ProjectRoot) {
				return errSessionIdentityConflict
			}
			// A Cursor path, once set, is never replaced by a different one,
			// and a session read from Cursor's database never takes one.
			if transcriptPath != "" && existing.ReadsTranscriptFile() && (!isCursor || existing.TranscriptPath == "") {
				existing.TranscriptPath = transcriptPath
			}
			existing.RegisteredAt = now
			applyHarnessObservation(&existing.Harness, harness, payload)
			return nil
		})
		switch {
		case errors.Is(err, errContinuationDeclined):
			return nil
		case err != nil:
			return err
		case updated:
			return saveLifecycleEvidence(store, existingID, harness, reason, payload, now)
		}
		// The index points at a registration we no longer have: it was never
		// eligible, or retention forgot it while this hook waited for the
		// lock. Either way this native session is treated as never seen, and
		// the fresh-start rules below decide whether it registers again.
	}

	// A directory inside no configured project, and an excluded project, are
	// both ignored without persisting their paths in diagnostics.
	if !owned || !owner.Included {
		return nil
	}
	// The diagnostic names the most specific reason capture was declined:
	// a project that is not yet active cannot capture any start, so check
	// activation before asking whether this start is provably fresh.
	if !cfg.Archive.Eligible(root, now) {
		return recordCaptureDiagnostic(home, captureDiagnostic{
			Code: diagnosticPreActivationStart, Harness: canonicalHarness(harness),
			ProjectRoot: root, ObservedAt: now,
		})
	}
	if !provesFreshSessionStart(harness, payload) {
		return recordCaptureDiagnostic(home, captureDiagnostic{
			Code: diagnosticUnknownSessionStart, Harness: canonicalHarness(harness),
			ProjectRoot: root, ObservedAt: now,
		})
	}
	observedHarness := archive.Harness{Name: strings.ToLower(strings.TrimSpace(harness))}
	applyHarnessObservation(&observedHarness, harness, payload)
	// RegisterNewSession saves under the archive ID's request lock and
	// rechecks the index there, so an index entry retention is removing
	// right now is never reused for a registration that would outlive it.
	reg, err := store.RegisterNewSession(nativeSessionID, func(archiveID string) archive.SessionRegistration {
		return archive.SessionRegistration{
			ArchiveSessionID: archiveID,
			NativeSessionID:  nativeSessionID,
			ProjectID:        archive.ProjectID(root),
			ProjectRoot:      root,
			Harness:          observedHarness,
			TranscriptPath:   transcriptPath,
			SessionStartedAt: now,
			RegisteredAt:     now,
			// A hook admits the session the moment it starts. A
			// continuation never rewrites these, so a hook resuming an
			// imported session keeps its import provenance.
			AdmittedAt:      now,
			Origin:          archive.SessionOriginHook,
			StartedAtSource: archive.StartedAtSourceHook,
			// Likewise the destination it was admitted into: a continuation
			// keeps it, and an older registration without one keeps none.
			DestinationID: cfg.DestinationID(),
		}
	})
	if err != nil {
		return fmt.Errorf("register session: %w", err)
	}
	return saveLifecycleEvidence(store, reg.ArchiveSessionID, harness, reason, payload, now)
}

var (
	// errContinuationDeclined: the registration exists but the current
	// configuration no longer accepts it. The hook records nothing.
	errContinuationDeclined = errors.New("continuation not accepted by the current configuration")
	// errSessionIdentityConflict: a different harness or configured project
	// claims an accepted registration's native session.
	errSessionIdentityConflict = errors.New("session identity conflicts with the accepted registration")
)

// configuredProjectActivationFor returns the configured project that owns
// root: the project whose root is root itself or its nearest configured
// ancestor, comparing resolved paths so a symlinked checkout still maps to the
// project it was registered under. The nearest ancestor wins, so a project
// nested inside another keeps its own identity (and its own inclusion
// decision). Excluded projects take part in the match: an excluded project
// nested in an included one must stay excluded rather than falling through to
// its parent.
func configuredProjectActivationFor(cfg config.Config, root string) (archive.ProjectActivation, bool) {
	if root == "" {
		return archive.ProjectActivation{}, false
	}
	candidate := resolvedPath(root)
	var best archive.ProjectActivation
	bestLen, found := -1, false
	for _, project := range cfg.Archive.Projects {
		configured := resolvedPath(project.Root)
		if !pathWithin(candidate, configured) || len(configured) <= bestLen {
			continue
		}
		best, bestLen, found = project, len(configured), true
	}
	return best, found
}

// configuredProjectFor returns the owning project's configured root spelling,
// which is what registrations store.
func configuredProjectFor(cfg config.Config, root string) (string, bool) {
	project, found := configuredProjectActivationFor(cfg, root)
	return project.Root, found
}

func resolvedPath(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// pathWithin reports whether path is root or lies beneath it.
func pathWithin(path, root string) bool {
	if path == root {
		return true
	}
	if !strings.HasSuffix(root, string(filepath.Separator)) {
		root += string(filepath.Separator)
	}
	return strings.HasPrefix(path, root)
}

func canonicalHarness(harness string) string {
	harness = strings.ToLower(strings.TrimSpace(harness))
	if harness == "claude-code" {
		return "claude"
	}
	return harness
}

// provesFreshSessionStart reports whether this SessionStart is provably the
// beginning of a conversation rather than the resumption of one that may
// predate the project's activation.
//
// Codex and Claude Code document SessionStart.source: startup and clear begin
// a conversation, resume and compact continue one. That evidence is decisive
// when it is present.
//
// Cursor carries no equivalent field, so the proof is the transcript itself:
// at the true start of a conversation the hook-provided transcript_path names
// a file that does not exist yet or holds no bytes, while a resumed
// conversation points at a transcript that already has content. Cursor's
// desktop app (observed on 3.21.13) goes further: a new chat's first
// beforeSubmitPrompt carries transcript_path null, and only afterAgentResponse
// and stop name the file, while a resumed chat's first prompt already names
// its existing transcript. So for Cursor a null or absent path is also proof.
// The file-based proof is the fallback for a Codex or Claude payload that
// carries no source at all; for those a payload that names no transcript
// proves nothing and is still declined.
func provesFreshSessionStart(harness string, payload map[string]any) bool {
	switch canonicalHarness(harness) {
	case "codex", "claude":
		switch strings.ToLower(strings.TrimSpace(firstNonEmptyString(payload, "source"))) {
		case "startup", "clear":
			return true
		case "":
			return emptyTranscriptProvesFreshStart(payload)
		}
		return false
	case "cursor":
		return cursorProvesFreshStart(payload)
	}
	return false
}

// cursorProvesFreshStart accepts a transcript_path that is null, absent, or
// empty — the shape of a new desktop chat's first prompt — or one that names a
// missing or empty file. A value that is present but not a string proves
// nothing.
func cursorProvesFreshStart(payload map[string]any) bool {
	value, present := payload["transcript_path"]
	if !present || value == nil {
		return true
	}
	path, ok := value.(string)
	if !ok {
		return false
	}
	if path == "" {
		return true
	}
	return emptyTranscriptProvesFreshStart(payload)
}

// emptyTranscriptProvesFreshStart reports whether the hook named a transcript
// that holds no conversation yet. Only "the file does not exist" and "the file
// is empty" are proof; a permission error, a directory, or anything else the
// hook cannot read leaves the start unproven. The transcript is never opened.
//
// Only an absolute path can be checked: a relative one would be resolved
// against the hook process's own working directory, where the transcript is
// never found, and "not found" would then pass as proof of a start that never
// happened. Harnesses document absolute transcript paths.
func emptyTranscriptProvesFreshStart(payload map[string]any) bool {
	path := firstNonEmptyString(payload, "transcript_path")
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return info.Size() == 0
}

func applyHarnessObservation(target *archive.Harness, harness string, payload map[string]any) {
	if target == nil {
		return
	}
	if strings.EqualFold(strings.TrimSpace(harness), "cursor") {
		if version := firstNonEmptyString(payload, "cursor_version"); version != "" {
			target.Version = version
		}
		if mode := firstNonEmptyString(payload, "composer_mode"); mode != "" {
			target.Mode = mode
		}
	}
}

// saveLifecycleEvidence records a start or prompt event as deferred evidence:
// it is folded into the next scheduled publication rather than forcing an
// upload on every prompt. Stop, end, and response events go through
// handleSessionStop, whose request is the intended debounce flush.
func saveLifecycleEvidence(store *collector.LocalStore, archiveID, harness, reason string, payload map[string]any, now time.Time) error {
	evidence, err := filteredHookEvidence(archive.EvidenceKindLifecycleHook, harness, reason, payload, false, now)
	if err != nil || evidence == nil {
		return err
	}
	return store.SaveEvidence(archiveID, reason, now, *evidence)
}

func handleSessionStop(store *collector.LocalStore, harness, nativeSessionID, eventName string, payload map[string]any, now time.Time) error {
	archiveID, found, err := store.ArchiveSessionID(nativeSessionID)
	if err != nil {
		return fmt.Errorf("look up archive session ID: %w", err)
	}
	if !found {
		// Never registered (ineligible project, or a resume we declined to
		// track): nothing to request.
		return nil
	}
	registration, registered, err := store.LoadRegistration(archiveID)
	if err != nil {
		return err
	}
	if !registered {
		return nil
	}
	if canonicalHarness(registration.Harness.Name) != canonicalHarness(harness) {
		return fmt.Errorf("session event does not match the accepted harness")
	}
	//lint:ignore LV1001 eventName is the hook_event_name a harness sends; any other name is expected and ignored
	if eventName == "afterAgentResponse" || eventName == "stop" {
		if err := adoptCursorTranscriptPath(store, &registration, harness, payload); err != nil {
			return err
		}
	}
	reason := strings.ToLower(eventName)
	var evidence []archive.SupplementalEvidence
	if isSessionLifecycleEvent(eventName) {
		lifecycle, err := filteredHookEvidence(archive.EvidenceKindLifecycleHook, harness, eventName, payload, false, now)
		if err != nil {
			return err
		}
		if lifecycle != nil {
			evidence = append(evidence, *lifecycle)
		}
	}
	filtered, err := filteredHookEvidence(archive.EvidenceKindFinalResponse, harness, eventName, payload, true, now)
	if err != nil {
		return err
	}
	if filtered != nil {
		evidence = append(evidence, *filtered)
	}
	return store.SaveRequest(archiveID, reason, now, evidence...)
}

func isSessionLifecycleEvent(eventName string) bool {
	//lint:ignore LV1001 eventName is the hook_event_name a harness sends; any other name is expected and ignored
	switch eventName {
	case "Stop", "Interrupt", "SessionEnd", "StopFailure", "stop", "sessionEnd":
		return true
	default:
		return false
	}
}

// extractHookEvidencePayload passes through only documented or stable identity
// fields. Cursor's generation_id is normalized to turn_id for reconciliation;
// every other value remains exactly as observed.
func extractHookEvidencePayload(payload map[string]any, includeFinalText bool) map[string]any {
	out := map[string]any{}
	if eventName := firstNonEmptyString(payload, "hook_event_name"); eventName != "" {
		out["event_name"] = eventName
	}
	for _, key := range []string{"message_id", "turn_id", "agent_id", "model", "model_id"} {
		if value, ok := payload[key].(string); ok && value != "" {
			out[key] = value
		}
	}
	if out["turn_id"] == nil {
		if generation := firstNonEmptyString(payload, "generation_id"); generation != "" {
			out["turn_id"] = generation
		}
	}
	if raw, ok := payload["model_params"].([]any); ok {
		params := make([]any, 0, len(raw))
		for _, item := range raw {
			param, ok := item.(map[string]any)
			if !ok {
				continue
			}
			id, value := firstNonEmptyString(param, "id"), firstNonEmptyString(param, "value")
			if id != "" && value != "" {
				params = append(params, map[string]any{"id": id, "value": value})
			}
		}
		if len(params) > 0 {
			out["model_params"] = params
		}
	}
	if includeFinalText {
		if text := firstNonEmptyString(payload, "last_assistant_message", "text"); text != "" {
			out["text"] = text
		}
	}
	return out
}

func filteredHookEvidence(kind archive.SupplementalEvidenceKind, harness, event string, payload map[string]any, includeFinalText bool, now time.Time) (*archive.SupplementalEvidence, error) {
	hookPayload := extractHookEvidencePayload(payload, includeFinalText)
	if kind == archive.EvidenceKindLifecycleHook {
		if status := documentedLifecycleStatus(harness, event, payload); status != "" {
			hookPayload["status"] = status
		}
	}
	if len(hookPayload) == 0 {
		return nil, nil
	}
	candidate := archive.SupplementalEvidence{
		Kind: kind, ObservedAt: now, Provenance: "hook:" + strings.ToLower(strings.TrimSpace(harness)) + ":" + strings.ToLower(event), Payload: hookPayload,
	}
	filtered, gaps, err := archive.FilterSupplementalEvidence([]archive.SupplementalEvidence{candidate})
	if err != nil {
		return nil, fmt.Errorf("filter hook evidence: %w", err)
	}
	if len(filtered) == 0 {
		return nil, nil
	}
	archive.AnnotateSupplementalGaps(filtered[0].Payload, gaps)
	return &filtered[0], nil
}

// cursorLifecycleStatus is a documented value of Cursor's stop status or
// sessionEnd reason.
type cursorLifecycleStatus string

const (
	cursorStatusCompleted   cursorLifecycleStatus = "completed"
	cursorStatusAborted     cursorLifecycleStatus = "aborted"
	cursorStatusError       cursorLifecycleStatus = "error"
	cursorStatusWindowClose cursorLifecycleStatus = "window_close"
	cursorStatusUserClose   cursorLifecycleStatus = "user_close"
)

// documentedLifecycleStatus retains only closed native enums. Free-form
// reason/status text is never archived as lifecycle metadata.
func documentedLifecycleStatus(harness, event string, payload map[string]any) string {
	if !strings.EqualFold(strings.TrimSpace(harness), "cursor") {
		return ""
	}
	var field cursorLifecycleStatus
	//lint:ignore LV1001 event is the hook_event_name Cursor sends, or a lowercased start or prompt reason; other values carry no status
	switch event {
	case "stop":
		field = cursorLifecycleStatus(firstNonEmptyString(payload, "status"))
	case "sessionEnd":
		field = cursorLifecycleStatus(firstNonEmptyString(payload, "reason"))
	default:
		return ""
	}
	switch field {
	case cursorStatusCompleted, cursorStatusAborted, cursorStatusError:
		return string(field)
	case cursorStatusWindowClose, cursorStatusUserClose:
		if event == "sessionEnd" {
			return string(field)
		}
	}
	return ""
}

func firstNonEmptyString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

// projectRoot reads cwd, falling back to the first entry of workspace_roots
// (Cursor's base hook schema uses that instead of a single cwd).
func projectRoot(payload map[string]any) string {
	if cwd := firstNonEmptyString(payload, "cwd"); cwd != "" {
		return cwd
	}
	if roots, ok := payload["workspace_roots"].([]any); ok {
		for _, raw := range roots {
			if root, ok := raw.(string); ok && root != "" {
				return root
			}
		}
	}
	return ""
}
