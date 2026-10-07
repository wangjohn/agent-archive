package agentapi

import (
	"context"
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
	NativeID string
	Cwd      string
}

// SourceAdmissionValidator checks opened source facts without reopening a locator.
type SourceAdmissionValidator interface {
	ValidateAdmission(context.Context, SourceAdmission) error
}

// CodexValidationLimits bounds use after one completed validation sweep. Zero
// values select 512 calls and 30 seconds; maxima are 512 calls and 60 seconds.
type CodexValidationLimits struct {
	Steps    int
	Duration time.Duration
}

// CodexRolloutSliceProvider optionally shares validation within a caller-owned
// operation slice. The caller closes all snapshots before renewing a slice.
type CodexRolloutSliceProvider interface {
	BeginValidationSlice(context.Context, CodexValidationLimits) (CodexRolloutSlice, error)
}

// CodexRolloutSlice expires without automatic renewal. It never supplies capture
// permission; providers still validate selected files on the opened handle.
type CodexRolloutSlice interface {
	CodexRolloutLookup
	Valid(context.Context) error
	Close() error
}
