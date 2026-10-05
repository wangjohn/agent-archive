package archive

import (
	"regexp"
	"strings"
)

// ReplayEnv is the environment variable a tool that replays archived tasks
// sets for the coding agents it runs, so the hooks that capture those runs
// mark them as replays rather than as the person's own work. Its value is
// the replay run's identifier (see IsReplayRunID); any other non-empty value
// still marks the session, without an identifier.
const ReplayEnv = "AGENT_ARCHIVE_REPLAY"

// replayRunID is the shape of a run identifier that is recorded: an opaque
// token, never free text.
var replayRunID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// IsReplayRunID reports whether s is a replay run identifier agent-archive
// records: 1 to 128 ASCII letters, digits, '.', '_', ':' or '-', starting
// with a letter or digit.
func IsReplayRunID(s string) bool { return replayRunID.MatchString(s) }

// Replay marks a session a replay tool ran (ReplayEnv was set when its
// hook registered it). A nil *Replay is an ordinary session.
type Replay struct {
	// RunID is the replay run's identifier, as the tool set it. Empty when
	// the variable held something that is not an identifier.
	RunID string `json:"run_id,omitempty"`
}

// ParseReplay reads ReplayEnv's value: nil when it is empty or only spaces
// (unset), a Replay with the value as RunID when it is an identifier, and a
// Replay without one otherwise, so a malformed value still keeps the run out
// of the person's history without recording the value.
func ParseReplay(value string) *Replay {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if IsReplayRunID(value) {
		return &Replay{RunID: value}
	}
	return &Replay{}
}

// IsReplay reports whether the metadata describes a replay session.
func (m *Metadata) IsReplay() bool { return m.Replay != nil }

// ApplyReplay copies the registration's replay marker into the metadata. A
// run identifier that is not IsReplayRunID is dropped (the marker stays), so
// nothing but an identifier reaches the sidecar through this field.
func (m *Metadata) ApplyReplay(r SessionRegistration) {
	if r.Replay == nil {
		m.Replay = nil
		return
	}
	var runID string
	if IsReplayRunID(r.Replay.RunID) {
		runID = r.Replay.RunID
	}
	m.Replay = &Replay{RunID: runID}
}
