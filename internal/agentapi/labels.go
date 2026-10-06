package agentapi

import (
	"context"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// LabelEnvironment supplies approved native homes without granting admission.
type LabelEnvironment struct {
	Homes []string `json:"homes"`
	// VerifiedLegacyStorageHomes carries positive per-home proof that the producing
	// legacy session used default SQLite placement. Local config absence is not proof.
	VerifiedLegacyStorageHomes []string `json:"verified_legacy_storage_homes"`
	ExternalSQLite             bool     `json:"external_sqlite"`
}

// LabelRequest identifies an already admitted retained source.
type LabelRequest struct {
	Registration archive.SessionRegistration
	Bundle       archive.SourceBundle
	Context      LabelContext
}

// LabelContext is durable, content-free interpretation of an owning retained source.
type LabelContext struct {
	NativeID        string `json:"native_id"`
	Ordinary        bool   `json:"ordinary"`
	Legacy          bool   `json:"legacy,omitempty"`
	Producer        string `json:"producer,omitempty"`
	PreviewDigest   string `json:"preview_digest,omitempty"`
	PreviewComplete bool   `json:"preview_complete,omitempty"`
	Contract        string `json:"contract"`
}

// LabelContextProvider derives narrow lookup facts once per retained source revision.
type LabelContextProvider interface {
	LabelContext(archive.SourceBundle) LabelContext
	// LabelContextVersion identifies the provider interpretation without performing I/O.
	LabelContextVersion() string
}

// LabelProvider resolves bounded metadata for existing sources without enumeration.
type LabelProvider interface {
	LookupLabels(context.Context, LabelEnvironment, []LabelRequest) map[string]archive.SessionLabel
}

// LabelsLookup projects only the optional native label capability.
type LabelsLookup interface {
	LookupLabels(string) (LabelProvider, bool)
}

// LabelRequestGrouper identifies a provider's shared work unit without I/O.
// The key is an opaque content-free SHA-256 hash; an empty key defers to ID priority.
type LabelRequestGrouper interface {
	LabelRequestGroup(LabelEnvironment, LabelRequest) string
}
