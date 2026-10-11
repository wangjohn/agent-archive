package codex

import (
	"errors"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
)

const missingDependency = "history dependency missing or ambiguous"

// refusal is the native message of err's first SourceError.
func refusal(err error) string {
	var e *agentapi.SourceError
	if !errors.As(err, &e) || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

// A history the lookup's rollouts do not resolve (a missing dependency, no
// single tip, no original segment) is a coverage refusal while coverage is
// unproven: incomplete, or a retained proof with no current locator. The
// same refusal on a proof of this pass is a failure.
func TestUnresolvedHistoryNamesCoverageOnlyWhileUnproven(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, raw := historyFile(t, dir, threadA, threadA, 0, nil, "before")
	position := codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}
	leaf, _ := historyFile(t, dir, rolloutC, threadA, 2, map[string]any{"history_base": position}, "after")
	other, _ := historyFile(t, dir, rolloutD, threadA, 2, map[string]any{"history_base": position}, "elsewhere")
	read := func(lookup *historyLookup, ref agentapi.SourceRef) error {
		t.Helper()
		s, e := historyPass(t, dir, lookup).Read(t.Context(), ref, agentapi.ReadLimits{})
		if e == nil {
			_ = s.Close()
		}
		return e
	}
	tips := []agentapi.SourceRef{leaf, other, base}
	for _, tc := range []struct {
		name     string
		lookup   *historyLookup
		ref      agentapi.SourceRef
		refusal  string
		coverage bool
	}{
		// Rollout(base) finds nothing before coverage has seen the base.
		{"dependency unseen while incomplete", &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf}}, leaf, missingDependency, true},
		{"dependency missing on a complete proof", &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf, Complete: true}}, leaf, missingDependency, false},
		{"tips ambiguous on a retained proof", &historyLookup{thread: agentapi.CodexRolloutSet{Complete: true, Retained: true, Candidates: tips}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}, base, "ambiguous rollout tips", true},
		{"tips ambiguous on this pass's proof", &historyLookup{thread: agentapi.CodexRolloutSet{Complete: true, Candidates: tips}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}, base, "ambiguous rollout tips", false},
		{"dependency missing on a retained proof", &historyLookup{thread: agentapi.CodexRolloutSet{Complete: true, Retained: true, Candidates: []agentapi.SourceRef{leaf}}}, base, missingDependency, true},
	} {
		e := read(tc.lookup, tc.ref)
		if e == nil || agentapi.Failure(e) != agentapi.Unavailable || refusal(e) != tc.refusal {
			t.Errorf("%s: %v (refusal %q, want %q)", tc.name, e, refusal(e), tc.refusal)
			continue
		}
		if named := coverageCause(e) == agentapi.CauseHistoryCoverage; named != tc.coverage {
			t.Errorf("%s: %v names coverage %v, want %v", tc.name, e, named, tc.coverage)
		}
	}
}

// A revision whose spans hold no segment of the thread's own, with no known
// binding, has no original creation facts until coverage finds that segment.
func TestMissingOriginalFactsNameCoverageOnlyWhileUnproven(t *testing.T) {
	t.Parallel()
	for _, complete := range []bool{false, true} {
		dir := t.TempDir()
		base, raw := historyFile(t, dir, threadB, threadB, 0, nil, "before")
		leaf, _ := historyFile(t, dir, rolloutC, threadA, 2, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: threadB, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}}, "after")
		lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf, Complete: complete}, rollouts: map[string][]agentapi.SourceRef{threadB: {base}}}
		s, e := historyPass(t, dir, lookup).Read(t.Context(), leaf, agentapi.ReadLimits{})
		if e != nil {
			t.Fatalf("complete=%v: %v", complete, e)
		}
		evidence, ok := s.(agentapi.SourceAdmissionEvidence)
		if !ok {
			t.Fatal("history snapshot gathers no admission evidence")
		}
		_, e = evidence.AdmissionEvidence(t.Context(), agentapi.SourceAdmission{NativeID: threadA})
		if closeErr := s.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		named := coverageCause(e) == agentapi.CauseHistoryCoverage
		if agentapi.Failure(e) != agentapi.Unavailable || refusal(e) != "original native creation facts unavailable" || named == complete {
			t.Errorf("complete=%v: %v (cause %q)", complete, e, coverageCause(e))
		}
	}
}
