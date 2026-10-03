package nativecodec

import (
	"github.com/wangjohn/agent-archive/internal/archive"
	"testing"
)

func TestEvalExportBranchExcludesParentSidechain(t *testing.T) {
	bundle := SourceBundle{NativeRecords: []map[string]any{{"isSidechain": true, "gitBranch": "child"}, {"gitBranch": "parent"}}}
	if got := exportFirstBranch(bundle); got != "parent" {
		t.Fatalf("parent branch = %q", got)
	}
	bundle.ParentSessionID = "parent-id"
	if got := exportFirstBranch(bundle); got != "child" {
		t.Fatalf("child branch = %q", got)
	}
}

func exportFirstBranch(bundle SourceBundle) string {
	facts := archive.NativeFacts{}
	for _, r := range bundle.NativeRecords {
		collectExportFacts(&facts, bundle, r, profileClaude)
	}
	return facts.FirstBranch
}
