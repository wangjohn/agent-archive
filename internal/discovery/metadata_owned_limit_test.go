package discovery

import (
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// A completed acquisition cannot retain its completeness claim after the
// owned epoch allowance is exhausted. Refusal still exposes no candidates.
func TestMetadataOwnedEpochLimitInvalidatesCompletedAcquisition(t *testing.T) {
	lookup, ids, _ := metadataFixture(t, 1)
	defer func() { _ = lookup.CloseReadOnly() }()
	view := lookup.MetadataInventory().(*metadataInventory)
	first, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Valid(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if !view.complete {
		t.Fatal("fixture acquisition incomplete before owned limit")
	}
	view.remaining = 0
	failed, err := view.BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = failed.Close() }()
	set, refusal := failed.Thread(t.Context(), ids[0])
	if agentapi.Failure(refusal) != agentapi.Limit || set.Complete || len(set.Candidates) != 0 || view.complete || view.cursor != nil || agentapi.Failure(view.failure) != agentapi.Limit {
		t.Fatalf("owned limit retained complete acquisition: set=%+v refusal=%v complete=%v cursor=%v failure=%v", set, refusal, view.complete, view.cursor != nil, view.failure)
	}
}
