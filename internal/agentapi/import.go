package agentapi

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/archive"
	"time"
)

// ImportStartPolicy declares evidence needed before filtering historical input.
type ImportStartPolicy uint8

const (
	ImportNativeStart ImportStartPolicy = iota
	ImportFileCreatedStart
)

// ImportPolicy declares native historical filtering requirements, not admission.
type ImportPolicy struct{ Start ImportStartPolicy }

// ImportInspectionRequest supplies retained filter observations and compact
// discovery evidence. Native identity rules remain separate from opaque keys.
type ImportInspectionRequest struct {
	Session  NativeSession
	Source   SourceRef
	Header   NativeHeader
	Filtered archive.FilteredTranscript
}

// ImportInspection contains native observations. Shared core chooses refusal,
// date/project filters, duplicate conflicts, registration and publication.
type ImportInspection struct {
	Conversation     bool
	IdentityMismatch bool
	StartedAt        time.Time
}

// ImportInspector owns native conversation, identity and start interpretation.
type ImportInspector interface {
	ImportPolicy(SourceRef) ImportPolicy
	InspectImport(context.Context, ImportInspectionRequest) (ImportInspection, error)
}

// ImportsLookup projects native historical observation owners to backfill.
type ImportsLookup interface {
	LookupImport(string) (ImportInspector, bool)
}
