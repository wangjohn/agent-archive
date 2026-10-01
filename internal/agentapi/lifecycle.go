package agentapi

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"time"
)

// HookInput carries local native JSON only during decoding.
type HookInput struct {
	Payload    map[string]any
	ObservedAt time.Time
}

// NativeSession contains exact native identity and observed native version facts.
type NativeSession struct {
	Agent                   agentmeta.ID
	NativeID, Version, Mode string
}

// EventKind is an archive-relevant lifecycle action, independent of native names.
type EventKind uint8

// The following values define the supported typed observations.
const (
	EventStart EventKind = iota + 1
	EventTurnStart
	EventResponse
	EventStop
	EventSubagent
)

// Freshness is native evidence interpreted by shared admission policy.
type Freshness uint8

// The following values define the supported typed observations.
const (
	FreshUnknown Freshness = iota
	FreshExplicit
	FreshContinuation
	FreshStat
)

// StartEvidence requests at most one bounded stat and never a source read.
type StartEvidence struct {
	Kind   Freshness
	Reason string
	Path   string
}

// LocatorUpdate declares a validated file locator's permitted update semantics.
type LocatorUpdate uint8

// The following values define the supported typed observations.
const (
	LocatorNone LocatorUpdate = iota
	LocatorReplaceFile
	LocatorFillFile
)

// DeferredKind limits which absent-parent effects may be retained for replay.
type DeferredKind uint8

// The following values define the supported typed observations.
const (
	DeferredNone DeferredKind = iota
	DeferredStart
	DeferredFollowup
)

// ChildObservation contains native child facts, without archive admission policy.
type ChildObservation struct {
	ID, Path, Type    string
	MissingDetail     string
	CaptureTranscript bool
}

// LifecycleEvent is a typed local result. Raw native payloads are never retained.
type LifecycleEvent struct {
	Kind        EventKind
	Session     NativeSession
	ProjectRoot string
	Source      SourceRef
	Locator     LocatorUpdate
	Start       StartEvidence
	NewOnly     bool
	Deferred    DeferredKind
	Reason      string
	NativeEvent string
	Evidence    []archive.SupplementalEvidence
	Child       *ChildObservation
}

// HookDecoder interprets native JSON; it performs no host operations.
type HookDecoder interface {
	Decode(context.Context, HookInput) ([]LifecycleEvent, error)
}

// DecodersLookup is the narrow port consumed by capture and replay.
type DecodersLookup interface {
	LookupDecoder(string) (HookDecoder, bool)
}

// LegacyAdmission retains the exact previous private intent wire fields.
// Only native decoders interpret their meaning; no raw hook content is present.
type LegacyAdmission struct {
	Harness         string    `json:"harness"`
	Event           string    `json:"event"`
	NativeSessionID string    `json:"native_session_id"`
	ProjectRoot     string    `json:"project_root"`
	DestinationID   string    `json:"destination_id"`
	PauseGeneration string    `json:"pause_generation,omitempty"`
	TranscriptPath  string    `json:"transcript_path,omitempty"`
	CursorVersion   string    `json:"cursor_version,omitempty"`
	ComposerMode    string    `json:"composer_mode,omitempty"`
	ObservedAt      time.Time `json:"observed_at"`
}

// LegacyHookDecoder translates old private records without rechecking fresh proof.
type LegacyHookDecoder interface {
	DecodeLegacy(AdmissionIntent) ([]LifecycleEvent, error)
}

// ReplayEffect is a content-free, versioned effect with original proof and identity.
type ReplayEffect struct{ Event LifecycleEvent }

// AdmissionIntent is the private replay wire envelope, retaining legacy fields.
type AdmissionIntent struct {
	Harness         string         `json:"harness"`
	Event           string         `json:"event"`
	NativeSessionID string         `json:"native_session_id"`
	ProjectRoot     string         `json:"project_root"`
	DestinationID   string         `json:"destination_id"`
	PauseGeneration string         `json:"pause_generation,omitempty"`
	TranscriptPath  string         `json:"transcript_path,omitempty"`
	CursorVersion   string         `json:"cursor_version,omitempty"`
	ComposerMode    string         `json:"composer_mode,omitempty"`
	ObservedAt      time.Time      `json:"observed_at"`
	Version         int            `json:"version,omitempty"`
	Effects         []ReplayEffect `json:"effects,omitempty"`
}
