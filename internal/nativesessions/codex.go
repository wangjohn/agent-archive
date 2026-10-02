package nativesessions

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type codexHistoryMode string

const (
	historyLegacy    codexHistoryMode = "legacy"
	historyPaginated codexHistoryMode = "paginated"
)

type codexExecutionSource string

const (
	executionCLI    codexExecutionSource = "cli"
	executionVSCode codexExecutionSource = "vscode"
)

type codexTaskEvent string

const (
	taskStarted codexTaskEvent = "task_started"
	turnStarted codexTaskEvent = "turn_started"
)

// CodexMeta is identity and execution metadata, never transcript body.
type CodexMeta struct {
	ID              string           `json:"id"`
	SessionID       string           `json:"session_id"`
	Timestamp       string           `json:"timestamp"`
	Cwd             string           `json:"cwd"`
	Source          json.RawMessage  `json:"source"`
	Originator      string           `json:"originator"`
	Version         string           `json:"cli_version"`
	ForkedFrom      json.RawMessage  `json:"forked_from_id"`
	ForkOrdinal     json.RawMessage  `json:"forked_from_ordinal_exclusive"`
	Parent          json.RawMessage  `json:"parent_thread_id"`
	HistoryBase     json.RawMessage  `json:"history_base"`
	HistoryMode     codexHistoryMode `json:"history_mode"`
	SubagentOrdinal json.RawMessage  `json:"subagent_history_start_ordinal"`
}

var uuid = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

var rolloutUUID = regexp.MustCompile(`(?i)([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)

// RolloutID returns a rollout filename's native identity.
func RolloutID(path string) string {
	m := rolloutUUID.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		return ""
	}
	return m[1]
}

// ParseCodexMeta decodes a session_meta record. Other records are ignored.
func ParseCodexMeta(line []byte) (CodexMeta, time.Time, bool, error) {
	var r struct {
		Type      string    `json:"type"`
		Timestamp string    `json:"timestamp"`
		Payload   CodexMeta `json:"payload"`
	}
	if err := json.Unmarshal(line, &r); err != nil {
		return CodexMeta{}, time.Time{}, false, err
	}
	if r.Type != "session_meta" {
		return CodexMeta{}, time.Time{}, false, nil
	}
	for _, ts := range []string{r.Payload.Timestamp, r.Timestamp} {
		if start, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			return r.Payload, start.UTC(), true, nil
		}
	}
	return r.Payload, time.Time{}, true, errors.New("invalid native start")
}

// ValidateIdentity requires matching bounded native and filename identities.
func (m CodexMeta) ValidateIdentity(path string) bool {
	return uuid.MatchString(m.ID) && strings.EqualFold(RolloutID(path), m.ID) && (m.SessionID == "" || m.SessionID == m.ID) && filepath.IsAbs(m.Cwd) && len(m.Cwd) <= 4096
}

func present(v json.RawMessage) bool { return len(v) > 0 && string(v) != "null" }

// Classification rejects inherited histories, child tasks and unknown source
// shapes. This is format classification, never proof a file was created here.
func (m CodexMeta) Classification() string {
	if present(m.ForkedFrom) || present(m.ForkOrdinal) || present(m.Parent) || present(m.HistoryBase) || present(m.SubagentOrdinal) {
		return "inherited_history"
	}
	if m.HistoryMode != "" && m.HistoryMode != historyLegacy && m.HistoryMode != historyPaginated {
		return "unsupported_history"
	}
	if !m.LocalExecutionSource() {
		return "unsupported_execution"
	}
	if m.Version == "" || m.Originator == "" {
		return "unsupported_producer"
	}
	return "native_format"
}

// LocalExecutionSource recognizes supported local source format tags only;
// it does not establish local originating execution or producer support.
func (m CodexMeta) LocalExecutionSource() bool {
	var source codexExecutionSource
	return json.Unmarshal(m.Source, &source) == nil && (source == executionCLI || source == executionVSCode)
}

// NativeFirstTask checks the first task_started, distinguishing Codex's built
// in migrated-history events. Callers must not skip a rejected first event.
func NativeFirstTask(line []byte) (seen, native bool) {
	var envelope struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(line, &envelope) != nil {
		return true, false
	}
	if envelope.Type != "event_msg" {
		return false, false
	}
	var kind struct {
		Type codexTaskEvent `json:"type"`
	}
	if json.Unmarshal(envelope.Payload, &kind) != nil || kind.Type == "" {
		return true, false
	}
	if kind.Type != taskStarted && kind.Type != turnStarted {
		return false, false
	}
	// This IS the first start even when its identity fields have invalid JSON
	// types. A later native-looking resume must never repair inherited history.
	var task struct {
		TurnID     string          `json:"turn_id"`
		RootTurnID string          `json:"root_turn_id"`
		StartedAt  json.RawMessage `json:"started_at"`
	}
	if json.Unmarshal(envelope.Payload, &task) != nil {
		return true, false
	}
	return true, uuid.MatchString(task.TurnID) && uuid.MatchString(task.RootTurnID) && !taskStartedAt(task.StartedAt).IsZero()
}

func taskStartedAt(raw json.RawMessage) time.Time {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		at, _ := time.Parse(time.RFC3339Nano, text)
		return at
	}
	seconds, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || seconds <= 0 || seconds > 253402300799 {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}

// FirstTaskAt returns only the first start's timestamp, without keeping body.
func FirstTaskAt(line []byte) time.Time {
	var r struct {
		Payload struct {
			StartedAt json.RawMessage `json:"started_at"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &r) != nil {
		return time.Time{}
	}
	return taskStartedAt(r.Payload.StartedAt)
}
