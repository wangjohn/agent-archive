// Package archive contains the privacy-first, storage-independent foundation
// for an Agent Archive collector. It deliberately has no filesystem, network,
// CLI, or credential dependencies.
package archive

import (
	"errors"
	"path/filepath"
	"strings"
	"time"
)

const (
	// SourceSchemaVersion 2 is the JSONL source bundle (source.<sha256>.jsonl.gz);
	// see source_jsonl.go. Schema 1, a single JSON document, is not read.
	SourceSchemaVersion   = 2
	MetadataSchemaVersion = 1
	// FilterVersion 3 keeps tool arguments and tool-result linkage, reports the
	// key names it could not keep, and strips injected instruction blocks from
	// message text. Filter 4 also retains Claude Code's isMeta flag and strips
	// the text of every isMeta record. Filter 5 retains Claude Code's
	// compact_boundary system record (type, subtype, ids, timestamp only) and
	// the isCompactSummary and isVisibleInTranscriptOnly flags, as booleans.
	// Filter 6 retains Claude Code's origin (as {kind}) and promptSource.
	// Filter 7 sanitizes a Cursor text transcript per role section, so the
	// 64 KB cap applies per message instead of truncating the whole
	// transcript, and bounds it by MaxRecordBytes instead of 2 MB. JSONL
	// output is unchanged. See docs/agent-archive-privacy.md.
	FilterVersion = "7"
	// OpenTelemetryGenAIRevision pins the upstream definitions used by the
	// three gen_ai.* attributes emitted by BuildMetadata. The archive is not
	// an OTLP payload; all agent_archive.* attributes are local extensions.
	OpenTelemetryGenAIRevision = "open-telemetry/semantic-conventions@aec6e9d3e86754683dab7c707655d69d953b2768 (v1.37.0)"
)

// ParserStatus reports how completely a metadata derivation covered its
// source. See schemas/metadata.schema.json for the authoritative enum.
type ParserStatus string

const (
	ParserStatusPartial  ParserStatus = "partial"
	ParserStatusFailed   ParserStatus = "failed"
	ParserStatusComplete ParserStatus = "complete"
)

// MetadataState is the session lifecycle state recorded on Metadata.
type MetadataState string

const (
	MetadataStateActive  MetadataState = "active"
	MetadataStateIdle    MetadataState = "idle"
	MetadataStateClosed  MetadataState = "closed"
	MetadataStateUnknown MetadataState = "unknown"
)

// TurnOutcome reports how the most recently observed turn ended. Completion
// means only that a supported native event explicitly reported completion; it
// is not a claim that the task succeeded.
type TurnOutcome string

const (
	TurnOutcomeCompleted   TurnOutcome = "completed"
	TurnOutcomeInterrupted TurnOutcome = "interrupted"
	TurnOutcomeError       TurnOutcome = "error"
	TurnOutcomeUnknown     TurnOutcome = "unknown"
)

// SkillDetection reports whether skill-usage evidence was observed for a
// session and, if so, whether any skill was actually used.
type SkillDetection string

const (
	// SkillDetectionUnavailable means the harness gave no skill-usage signal.
	SkillDetectionUnavailable SkillDetection = "unavailable"
	// SkillDetectionObserved means at least one skill was used.
	SkillDetectionObserved SkillDetection = "observed"
	// SkillDetectionObservedNone means skills were known to be available but
	// none were used. A reader's "eligible but unused" filter requires this
	// exact value; see reader.SkillUsageEligibleNoUse.
	SkillDetectionObservedNone SkillDetection = "observed_none"
	// SkillDetectionPartial means skill-usage coverage was incomplete.
	SkillDetectionPartial SkillDetection = "partial"
)

// SkillCoverage classifies why a skill appears in Metadata.SkillsAvailable.
// Only SkillCoverageEligible and SkillCoverageDiscovered count as available
// for filtering; SkillCoverageInstalledOnly and any other hook-reported value
// do not.
type SkillCoverage string

const (
	SkillCoverageEligible      SkillCoverage = "eligible"
	SkillCoverageDiscovered    SkillCoverage = "discovered"
	SkillCoverageInstalledOnly SkillCoverage = "installed_only"
)

// SkillUseEvidence names how a skill use was detected.
type SkillUseEvidence string

const (
	SkillUseEvidenceNativeInvocation SkillUseEvidence = "native_invocation"
	SkillUseEvidenceReadInference    SkillUseEvidence = "skill_read_inference"
)

// SupplementalEvidenceKind names the recognized shapes of hook-only evidence.
// It is a named string, not a closed set: a hook may report a kind this
// package does not recognize, and such evidence is still preserved as-is.
type SupplementalEvidenceKind string

const (
	EvidenceKindSkillInventory   SupplementalEvidenceKind = "skill_inventory"
	EvidenceKindSkillDiscovered  SupplementalEvidenceKind = "skill_discovered"
	EvidenceKindSkillSnapshot    SupplementalEvidenceKind = "skill_snapshot"
	EvidenceKindSkillInvocation  SupplementalEvidenceKind = "skill_invocation"
	EvidenceKindSkillRead        SupplementalEvidenceKind = "skill_read"
	EvidenceKindLifecycleHook    SupplementalEvidenceKind = "lifecycle_hook"
	EvidenceKindFinalResponse    SupplementalEvidenceKind = "final_response"
	EvidenceKindExplicitFeedback SupplementalEvidenceKind = "explicit_feedback"
	EvidenceKindLinkedSession    SupplementalEvidenceKind = "linked_session"
	EvidenceKindCaptureGap       SupplementalEvidenceKind = "capture_gap"
)

// LinkedSessionStatus is the durable producer state for a child session.
// Readers resolve it against remote metadata to distinguish an upload still
// pending from a published child whose metadata is now unavailable.
type LinkedSessionStatus string

const (
	LinkedSessionPending     LinkedSessionStatus = "pending"
	LinkedSessionPublished   LinkedSessionStatus = "published"
	LinkedSessionUnavailable LinkedSessionStatus = "unavailable"
)

// LinkedSessionReference identifies a separately archived source. It carries
// no transcript content and never contributes to the containing session's
// counts.
type LinkedSessionReference struct {
	SessionID    string              `json:"session_id"`
	Relationship string              `json:"relationship"`
	Status       LinkedSessionStatus `json:"status"`
	ObservedAt   time.Time           `json:"observed_at"`
}

// ModelSummarySource names where a ModelSummary's attribution came from.
type ModelSummarySource string

const (
	ModelSummarySourceNativeTranscript ModelSummarySource = "native_transcript"
	ModelSummarySourceHook             ModelSummarySource = "hook"
)

// ResponseModelStatus reports whether a model attribution came from the
// request side (not exposed to the archive) or was directly observed on a
// response.
type ResponseModelStatus string

const (
	ResponseModelStatusNotExposed ResponseModelStatus = "not_exposed"
	ResponseModelStatusObserved   ResponseModelStatus = "observed"
)

// Config is the durable, non-secret configuration required to decide whether a
// session is eligible for capture. Credentials and runtime state do not belong
// in this type.
type Config struct {
	SchemaVersion int                 `json:"schema_version"`
	MachineID     string              `json:"machine_id"`
	Enabled       bool                `json:"enabled"`
	Projects      []ProjectActivation `json:"projects"`
}

// ProjectActivation explicitly opts a project into capture. Sessions which
// started before ActivatedAt are intentionally ineligible.
type ProjectActivation struct {
	ProjectID   string    `json:"project_id"`
	Root        string    `json:"root"`
	ActivatedAt time.Time `json:"activated_at"`
	Included    bool      `json:"included"`
}

// Eligible reports whether a project is explicitly included and the session
// began at or after its activation time. It performs lexical path matching; a
// caller is responsible for resolving symlinks before constructing Config.
func (c Config) Eligible(projectRoot string, sessionStartedAt time.Time) bool {
	if !c.Enabled || sessionStartedAt.IsZero() {
		return false
	}
	clean := filepath.Clean(projectRoot)
	for _, project := range c.Projects {
		if project.Included && filepath.Clean(project.Root) == clean && !project.ActivatedAt.IsZero() && !sessionStartedAt.Before(project.ActivatedAt) {
			return true
		}
	}
	return false
}

// Harness identifies the application which owns the native transcript.
type Harness struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Mode    string `json:"mode,omitempty"`
}

// SessionOrigin is how a session entered the archive. The empty value means
// SessionOriginHook, so registrations written before the field decode as hook
// registrations.
type SessionOrigin string

const (
	SessionOriginHook   SessionOrigin = "hook"
	SessionOriginImport SessionOrigin = "import"
)

// StartedAtSource says where a registration's SessionStartedAt came from. The
// empty value means StartedAtSourceHook.
type StartedAtSource string

const (
	StartedAtSourceHook           StartedAtSource = "hook"            // when the hook fired
	StartedAtSourceTranscript     StartedAtSource = "transcript"      // earliest native record
	StartedAtSourceFileCreated    StartedAtSource = "file_created"    // file birth time; format has no timestamps
	StartedAtSourceCursorComposer StartedAtSource = "cursor_composer" // phase 2: composerData.createdAt
)

// SessionRegistration is the small hook-produced observation a later collector
// needs. TranscriptPath is local operational data and is never placed in a
// SourceBundle or Metadata document.
//
// SessionStartedAt is when the conversation began. It is not the capture
// boundary: project activation and the storage destination compare Admitted().
type SessionRegistration struct {
	ArchiveSessionID      string    `json:"archive_session_id"`
	NativeSessionID       string    `json:"native_session_id"`
	ProjectID             string    `json:"project_id"`
	ProjectRoot           string    `json:"project_root"`
	Harness               Harness   `json:"harness"`
	TranscriptPath        string    `json:"transcript_path"`
	SessionStartedAt      time.Time `json:"session_started_at"`
	RegisteredAt          time.Time `json:"registered_at"`
	ParentSessionID       string    `json:"parent_session_id,omitempty"`
	ParentNativeSessionID string    `json:"parent_native_session_id,omitempty"`
	SubagentID            string    `json:"subagent_id,omitempty"`
	SubagentObservedAt    time.Time `json:"subagent_observed_at,omitempty"`
	// AdmittedAt is when this machine took ownership of the session: the
	// boundary for project activation and storage destination. Hooks set it at
	// registration and backfill sets it to the import time. Empty on older
	// registrations.
	AdmittedAt time.Time `json:"admitted_at,omitempty"`
	// Origin is how the session entered the archive. Set once.
	Origin SessionOrigin `json:"origin,omitempty"`
	// StartedAtSource says where SessionStartedAt came from.
	StartedAtSource StartedAtSource `json:"started_at_source,omitempty"`
	// ImportBatch is the backfill run that registered the session.
	ImportBatch string `json:"import_batch,omitempty"`
	// SourceKind is where the collector reads the session from, fixed at
	// registration: a transcript file ("") or a Cursor database chat.
	SourceKind SourceKind `json:"source_kind,omitempty"`
	// SourceKey names the session within its source: for
	// SourceKindCursorSQLite, the chat's composer ID.
	SourceKey string `json:"source_key,omitempty"`
}

// Admitted is the boundary time. Registrations older than AdmittedAt were
// all hook-registered, so their start is their admission.
func (r SessionRegistration) Admitted() time.Time {
	if !r.AdmittedAt.IsZero() {
		return r.AdmittedAt
	}
	return r.SessionStartedAt
}

// Imported reports whether backfill, not a hook, registered the session.
func (r SessionRegistration) Imported() bool {
	return r.Origin == SessionOriginImport
}

func (r SessionRegistration) Validate() error {
	if strings.TrimSpace(r.ArchiveSessionID) == "" || strings.TrimSpace(r.NativeSessionID) == "" {
		return errors.New("archive and native session IDs are required")
	}
	if strings.TrimSpace(r.ProjectID) == "" || strings.TrimSpace(r.ProjectRoot) == "" {
		return errors.New("project ID and project root are required")
	}
	if strings.TrimSpace(r.Harness.Name) == "" {
		return errors.New("harness name is required")
	}
	if r.SessionStartedAt.IsZero() {
		return errors.New("session start time is required; older sessions cannot be inferred safely")
	}
	return r.validateSource()
}

// CaptureGap tells readers why source coverage is incomplete without including
// an omitted value or transcript content.
type CaptureGap struct {
	Code   string `json:"code"`
	Record int    `json:"record,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// CaptureBoundary describes retained evidence represented by a source snapshot.
// Excluded native records deliberately do not affect it or a source hash.
type CaptureBoundary struct {
	RetainedRecords int `json:"retained_records"`
	RetainedBytes   int `json:"retained_bytes"`
}

// FilteredTranscript is the only adapter output accepted by NewSourceBundle.
// Records retain their allowed native JSON shape and source ordering.
type FilteredTranscript struct {
	Format              string          `json:"format"`
	Records             [][]byte        `json:"-"`
	Boundary            CaptureBoundary `json:"boundary"`
	Gaps                []CaptureGap    `json:"gaps,omitempty"`
	FirstEventAt        time.Time       `json:"-"`
	Text                []string        `json:"-"`
	SessionIDs          []string        `json:"-"`
	AgentIDs            []string        `json:"-"`
	NativeStartAt       time.Time       `json:"-"`
	NativeEndAt         time.Time       `json:"-"`
	NativeStartComplete bool            `json:"-"`
}

// SupplementalEvidence is hook-only evidence. Payload must already be
// privacy-filtered by the producer; this package preserves it separately from
// native records and does not reconcile it into a transcript.
type SupplementalEvidence struct {
	Kind       SupplementalEvidenceKind `json:"kind"`
	ObservedAt time.Time                `json:"observed_at"`
	Provenance string                   `json:"provenance"`
	Payload    map[string]any           `json:"payload"`
}

// SourceCapture describes the provenance shared by all records in a snapshot.
type SourceCapture struct {
	Harness        Harness         `json:"harness"`
	AdapterName    string          `json:"adapter_name"`
	AdapterVersion string          `json:"adapter_version"`
	SourceFormat   string          `json:"source_format"`
	Boundary       CaptureBoundary `json:"boundary"`
	FilterVersion  string          `json:"filter_version"`
	CapturedAt     time.Time       `json:"captured_at"`
	Gaps           []CaptureGap    `json:"gaps,omitempty"`
}

// SourceBundle is the durable filtered source envelope. It intentionally has
// no normalized transcript or local filesystem path.
type SourceBundle struct {
	SchemaVersion        int                      `json:"schema_version"`
	ArchiveSessionID     string                   `json:"archive_session_id"`
	NativeSessionID      string                   `json:"native_session_id"`
	ProjectID            string                   `json:"project_id"`
	Capture              SourceCapture            `json:"capture"`
	NativeRecords        []map[string]any         `json:"native_records"`
	NativeText           []TextTranscript         `json:"native_text,omitempty"`
	SupplementalEvidence []SupplementalEvidence   `json:"supplemental_evidence,omitempty"`
	ParentSessionID      string                   `json:"parent_session_id,omitempty"`
	LinkedSessions       []LinkedSessionReference `json:"linked_sessions,omitempty"`
}

// TextTranscript preserves a safely filtered native text transcript without
// inventing JSONL records or universal message events.
type TextTranscript struct {
	Format  string `json:"format"`
	Content string `json:"content"`
}

// SourceReference is the content-addressed pointer carried by metadata.
type SourceReference struct {
	Key             string `json:"key"`
	SHA256          string `json:"sha256"`
	CompressedBytes int    `json:"compressed_bytes"`
}

// AdapterInfo records which code filtered and summarized the source.
type AdapterInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ParserInfo identifies a metadata derivation. Status is partial, failed, or
// complete only when the particular adapter has demonstrated complete coverage.
type ParserInfo struct {
	Name    string       `json:"name"`
	Version string       `json:"version"`
	Status  ParserStatus `json:"status"`
}

// SemanticConventionsInfo identifies the exact upstream vocabulary revision
// used for matching attribute names. Local extensions use agent_archive.*.
type SemanticConventionsInfo struct {
	Name     string `json:"name"`
	Revision string `json:"revision"`
}

// Counts intentionally uses pointers: nil means unavailable, rather than an
// invented zero after a partial or failed parse.
//
// Turns counts human prompts only: a user record which carries text or any
// non-tool-result content and is not harness-written (an isMeta record, local
// command output, a `!` shell command, or a slash command no assistant
// answered). Messages counts those prompts plus distinct assistant messages:
// Claude Code streams one response as several records sharing message.id,
// which count once; a record without an id counts on its own. Assistant
// records whose only content is a tool call are included.
// ToolResults counts observed tool results, which are the records a harness
// writes in the user role but no human sent. The token counts are summed from
// whatever the harness exposed (Claude `message.usage`, Codex
// `turn_token_usage`) and stay nil when it exposed nothing; usage repeated
// across the streamed records of one Claude `message.id` is counted once.
type Counts struct {
	Turns       *int `json:"turns,omitempty"`
	Messages    *int `json:"messages,omitempty"`
	ToolCalls   *int `json:"tool_calls,omitempty"`
	ToolResults *int `json:"tool_results,omitempty"`
	// UserShellCommands counts shell commands the person ran directly (Claude
	// Code's `!` commands, recorded as <bash-input>). They are neither prompts
	// nor model tool calls.
	UserShellCommands *int `json:"user_shell_commands,omitempty"`
	// Compactions counts how many times Claude Code compacted the session
	// (/compact or auto-compaction), from its compact_boundary records, or
	// from its compaction summaries when no boundary was retained. It is
	// known only for Claude Code bundles from filter 5 on.
	Compactions      *int `json:"compactions,omitempty"`
	ExplicitFeedback *int `json:"explicit_feedback,omitempty"`
	InputTokens      *int `json:"input_tokens,omitempty"`
	OutputTokens     *int `json:"output_tokens,omitempty"`
	CacheReadTokens  *int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int `json:"cache_write_tokens,omitempty"`
}

type ModelSummary struct {
	Attributes          map[string]string   `json:"attributes"`
	Source              ModelSummarySource  `json:"source"`
	ResponseModelStatus ResponseModelStatus `json:"response_model_status"`
	TurnCount           *int                `json:"turn_count,omitempty"`
}

type SkillSnapshot struct {
	Name     string        `json:"name"`
	SHA256   string        `json:"sha256,omitempty"`
	Coverage SkillCoverage `json:"coverage,omitempty"`
}

type SkillUse struct {
	Name      string           `json:"name"`
	SHA256    string           `json:"sha256,omitempty"`
	TurnCount *int             `json:"turn_count,omitempty"`
	Evidence  SkillUseEvidence `json:"evidence"`
}

// Metadata is the replaceable, source-first reader index. It contains no
// transcript text or tool payloads.
type Metadata struct {
	SchemaVersion       int                      `json:"schema_version"`
	SessionID           string                   `json:"session_id"`
	NativeSessionID     string                   `json:"native_session_id"`
	MachineID           string                   `json:"machine_id"`
	ProjectID           string                   `json:"project_id"`
	StartedAt           time.Time                `json:"started_at"`
	CapturedAt          time.Time                `json:"captured_at"`
	MetadataDerivedAt   time.Time                `json:"metadata_derived_at"`
	Harness             Harness                  `json:"harness"`
	Adapter             AdapterInfo              `json:"adapter"`
	Parser              ParserInfo               `json:"parser"`
	FilterVersion       string                   `json:"filter_version"`
	State               MetadataState            `json:"state"`
	TurnOutcome         TurnOutcome              `json:"turn_outcome,omitempty"`
	SemanticConventions *SemanticConventionsInfo `json:"semantic_conventions,omitempty"`
	Models              []ModelSummary           `json:"models,omitempty"`
	SkillsAvailable     []SkillSnapshot          `json:"skills_available,omitempty"`
	SkillsUsed          []SkillUse               `json:"skills_used,omitempty"`
	SkillDetection      SkillDetection           `json:"skill_detection"`
	Counts              Counts                   `json:"counts"`
	CaptureGaps         []CaptureGap             `json:"capture_gaps,omitempty"`
	SourceBundle        SourceReference          `json:"source_bundle"`
	ParentSessionID     string                   `json:"parent_session_id,omitempty"`
	LinkedSessions      []LinkedSessionReference `json:"linked_sessions,omitempty"`
	// Origin, ImportedAt, and StartedAtSource describe a session backfill
	// imported. All three are omitted for hook-captured sessions, so their
	// metadata is unchanged. See ApplyRegistrationProvenance.
	Origin          SessionOrigin   `json:"origin,omitempty"`
	ImportedAt      *time.Time      `json:"imported_at,omitempty"`
	StartedAtSource StartedAtSource `json:"started_at_source,omitempty"`
}

// CaptureGapImportedWithoutHookEvidence marks an imported session: no hook
// ran while it happened, so it has no lifecycle events, final-response text,
// or skill inventory.
const CaptureGapImportedWithoutHookEvidence = "imported_without_hook_evidence"

// ApplyRegistrationProvenance records how the session entered the archive.
// It changes nothing for a hook registration. Callers apply it to every
// metadata document BuildMetadata returns, including a failed parse's.
func (m *Metadata) ApplyRegistrationProvenance(r SessionRegistration) {
	if !r.Imported() {
		return
	}
	m.Origin = r.Origin
	if !r.AdmittedAt.IsZero() {
		importedAt := r.AdmittedAt.UTC()
		m.ImportedAt = &importedAt
	}
	m.StartedAtSource = r.StartedAtSource
	for _, gap := range m.CaptureGaps {
		if gap.Code == CaptureGapImportedWithoutHookEvidence {
			return
		}
	}
	m.CaptureGaps = append(m.CaptureGaps, CaptureGap{Code: CaptureGapImportedWithoutHookEvidence, Detail: importedGapDetail})
}

// importedGapDetail is about the time before the import only: a hook that
// resumes an imported session records its lifecycle from then on.
const importedGapDetail = "No hook observed this session before it was imported (imported_at): activity before then has no hook lifecycle events, final-response text, or skill inventory."
