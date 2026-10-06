package backfill

import (
	"io"
	"sort"

	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// DiagnosticDetail is a fixed content-free classification, never source text.
type DiagnosticDetail string

// DiagnosticAction describes whether retry or a reviewed policy change can help.
type DiagnosticAction string

// Diagnostic actions never grant permission to import a source.
const (
	ActionRetry         DiagnosticAction = "retry"
	ActionReviewProject DiagnosticAction = "review_project"
	ActionReviewMapping DiagnosticAction = "review_mapping"
	ActionAwaitSupport  DiagnosticAction = "await_support"
)

// Diagnostic is a content-free explanation subordinate to the primary skip code.
// Detail is a fixed code, never a native ID, path, Git stderr or transcript field.
type Diagnostic struct {
	Detail DiagnosticDetail `json:"detail"`
	Action DiagnosticAction `json:"action"`
}

// DiagnosticSummary counts a fixed diagnostic without exposing individual sources.
type DiagnosticSummary struct {
	Skip SkipReason `json:"skip"`
	Diagnostic
	Count int `json:"count"`
}

type diagnosticDescription struct {
	action DiagnosticAction
	text   string
}

// The finite vocabulary bounds rendering even when thousands of sources fail.
var diagnosticDescriptions = map[DiagnosticDetail]diagnosticDescription{
	DiagnosticDetail(sourcefacts.RecoveryBudgetExhausted):       {ActionRetry, "Project recovery reached its resource limit; retry the plan."},
	DiagnosticDetail(sourcefacts.RecoveryInventoryUnavailable):  {ActionRetry, "Configured repository evidence is unavailable or changed; check repository access and Git, then retry."},
	DiagnosticDetail(sourcefacts.RecoveryExcluded):              {ActionReviewProject, "Recorded repository identity matches an excluded project; review the exclusion."},
	DiagnosticDetail(sourcefacts.RecoveryAmbiguous):             {ActionReviewMapping, "Recorded repository identity matches multiple configured repositories; review an exact --map-project assignment."},
	DiagnosticDetail(sourcefacts.RecoveryRepositoryUnavailable): {ActionReviewProject, "Recorded repository identity is missing or has no matching configured repository; review the project and an exact --map-project assignment."},
	DiagnosticDetail(sourcefacts.RecoveryMappingConflict):       {ActionReviewMapping, "The mapping conflicts with configured ownership or recorded repository identity; review the assignment."},
	DiagnosticDetail(sourcefacts.RecoverySubtreeUnavailable):    {ActionReviewMapping, "Repository identity cannot prove the original subtree's ownership; review project scope."},
	DiagnosticDetail(sourcefacts.RecoveryUnavailable):           {ActionReviewProject, "The original project evidence cannot be used safely; review the project."},
	"worktree_evidence_unavailable":                             {ActionReviewProject, "The worktree cannot be followed to its repository; review the checkout and project."},
	"history_lookup_pending":                                    {ActionAwaitSupport, "Related history remains pending; current selection and dependencies have not been inspected by backfill."},
	"source_changed":                                            {ActionRetry, "The source changed during planning; retry after it settles."},
	"unsupported_format":                                        {ActionAwaitSupport, "The privacy filter cannot safely read this format; support is required before import."},
	"source_size_limit":                                         {ActionAwaitSupport, "The source exceeds the supported size limit."},
}

func candidateDiagnostic(skip SkipReason, outcome sourcefacts.RecoveryOutcome) *Diagnostic {
	var detail DiagnosticDetail
	switch skip {
	case SkipWorktreeUnresolved:
		detail = DiagnosticDetail(outcome)
		if detail == "" {
			detail = "worktree_evidence_unavailable"
		}
	case SkipRelatedHistory:
		detail = "history_lookup_pending"
	case SkipSourceChanged:
		detail = "source_changed"
	case SkipUnsafeFormat:
		detail = "unsupported_format"
	case SkipTooLarge:
		detail = "source_size_limit"
	}
	description, ok := diagnosticDescriptions[detail]
	if !ok {
		return nil
	}
	return &Diagnostic{Detail: detail, Action: description.action}
}

// Diagnostics aggregates only known diagnostics belonging to the winning skip.
func (p Plan) Diagnostics() []DiagnosticSummary {
	counts := map[string]DiagnosticSummary{}
	for _, c := range p.Candidates {
		if c.Skip == "" || c.Diagnostic == nil {
			continue
		}
		d, ok := diagnosticDescriptions[c.Diagnostic.Detail]
		if !ok {
			continue
		}
		key := string(c.Skip) + "\x00" + string(c.Diagnostic.Detail)
		summary := counts[key]
		summary.Skip, summary.Detail, summary.Action = c.Skip, c.Diagnostic.Detail, d.action
		summary.Count++
		counts[key] = summary
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]DiagnosticSummary, 0, len(keys))
	for _, key := range keys {
		out = append(out, counts[key])
	}
	return out
}

func renderDiagnostics(w io.Writer, p Plan) {
	for _, d := range p.Diagnostics() {
		terminal.Printf(w, "      %d: %s\n", d.Count, diagnosticDescriptions[d.Detail].text)
	}
}

// InventoryAccounting reconciles unique physical candidate files, separately
// from database-only candidates. It does not claim logical-history accounting:
// dependencies, identical copies and retained revisions need the shared lookup.
type InventoryAccounting struct {
	UniqueCandidateFiles  int            `json:"unique_candidate_files"`
	DatabaseCandidates    int            `json:"database_candidates"`
	Dispositions          map[string]int `json:"dispositions"`
	LogicalHistoryPending bool           `json:"logical_history_pending"`
}

// InventoryAccounting assigns each observed physical candidate exactly one
// disposition. Duplicate candidates are not claimed to be identical copies.
func (p Plan) InventoryAccounting() InventoryAccounting {
	out := InventoryAccounting{Dispositions: map[string]int{}, LogicalHistoryPending: true}
	seen := map[string]bool{}
	for _, c := range p.Candidates {
		if c.TranscriptPath == "" {
			out.DatabaseCandidates++
			continue
		}
		if seen[c.TranscriptPath] {
			continue
		}
		seen[c.TranscriptPath] = true
		out.UniqueCandidateFiles++
		disposition := "unresolved"
		switch c.Skip {
		case "":
			disposition = "planned_import"
		case SkipDuplicateSession:
			disposition = "duplicate_candidate"
		case SkipAlreadyArchived:
			disposition = "already_archived"
		case SkipExcludedProject, SkipFilteredOut, SkipHomeDirectory, SkipAboveHome, SkipTemporaryDirectory, SkipRemovedByUndo, SkipRemovedByRetention:
			disposition = "excluded"
		}
		out.Dispositions[disposition]++
	}
	return out
}
