package agentapi

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/archive"
	"time"
)

// CodexRolloutLookup supplies bounded existing catalog evidence without enumeration.
// Every locator is an untrusted hint, revalidated by the source provider.
type CodexRolloutLookup interface {
	Rollout(context.Context, string) ([]SourceRef, error)
	Thread(context.Context, string) (CodexRolloutSet, error)
	Check(context.Context, string, string) error
}

// CodexRolloutSet separates a native current locator from complete lineage evidence.
// Complete must never be inferred from a prunable observation cache alone.
// Revision changes whenever the current locator, candidate membership, or candidate
// identity evidence changes. Check must revalidate this token within the same budget.
type CodexRolloutSet struct {
	Current    *SourceRef
	Candidates []SourceRef
	Revision   string
	Complete   bool
}

// CodexHistoryHeader and CodexHistoryRecord frame selected identity and physical records.
const (
	CodexHistoryHeader NativeRecordKind = "codex_history_header"
	CodexHistoryRecord NativeRecordKind = "codex_history_record"
)

// SourceAdmission contains immutable native observations, never capture permission.
type SourceAdmission struct {
	NativeID                  string
	Cwd                       string
	Binding                   *archive.CodexSourceBinding
	NativeCreatedAt           time.Time
	InitialProducerVersion    string
	InitialProducerOriginator string
	InitialProducerSource     string
}

// SourceAdmissionValidator checks opened source facts without reopening a locator.
type SourceAdmissionValidator interface {
	ValidateAdmission(context.Context, SourceAdmission) error
}

// SourceAdmissionFacts exposes bounded facts from the actual opened snapshot.
type SourceAdmissionFacts interface {
	AdmissionFacts(context.Context) (archive.CodexSourceBinding, error)
}

// SourceAdmissionSignature validates opened native facts without decoding a
// complete transcript merely to decide whether its signature is unchanged.
type SourceAdmissionSignature interface {
	ValidateSourceAdmission(context.Context, SourceRef, SourceAdmission) error
}

// SourceRevisions reads validated historical physical segments under existing
// thread admission, independently of which segment the native row selects now.
type SourceRevisions interface {
	ReadRevision(context.Context, SourceRef, SourceAdmission, ReadLimits) (SourceSnapshot, error)
}

// SourceRevisionCandidates exposes bounded same-thread candidates from validated
// graph and lookup evidence; these observations never grant capture admission.
type SourceRevisionCandidates interface {
	RevisionCandidates(context.Context) ([]SourceRef, error)
}

// OwnTaskFacts contains the first owned native task observation, never permission.
// Seen with Native false refuses a later task as replacement evidence.
type OwnTaskFacts struct {
	Seen           bool
	Native         bool
	LocalExecution bool
	StartedAt      time.Time
	TurnID         string
}

// SourceOwnTaskFacts observes the first task within the snapshot's own logical
// boundary, even when a copied model-context prefix exceeds a header window.
type SourceOwnTaskFacts interface {
	FirstOwnTask(context.Context) (OwnTaskFacts, error)
}

// AdmissionEvidence combines facts from one opened snapshot before admission.
type AdmissionEvidence struct {
	Binding archive.CodexSourceBinding
	Task    OwnTaskFacts
}

// SourceAdmissionEvidence gathers original facts and first-owned task evidence,
// validates supplied immutable constraints, and performs one final shared check.
type SourceAdmissionEvidence interface {
	AdmissionEvidence(context.Context, SourceAdmission) (AdmissionEvidence, error)
}
