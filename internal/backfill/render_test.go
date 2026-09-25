package backfill

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// Every skip reason renders with its wording and override, in the spec's
// order. Regenerate with `go test ./internal/backfill -run
// TestSkipReasonsGolden -update` and review the diff.
//
// Regression: backfill review, 2026-09 (cbce179).
func TestSkipReasonsGolden(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	p := Plan{
		GeneratedAt: fixedNow, Home: "/Users/p", RetentionDays: 90, Harnesses: []string{"claude"},
		Destination: Destination{Provider: "s3", Bucket: "bucket", Prefix: "agent-archive"},
		Candidates: []Candidate{
			{Harness: "claude", ProjectRoot: "/Users/p/repo", ProjectKind: ProjectKindRepository, ProjectExists: true, StartedAt: start, Bytes: 2000,
				Subagents: []Subagent{{AgentID: "a1", Bytes: 1000}}, SubagentsSkipped: 1},
			{Harness: "claude", ProjectRoot: "/Users/p", ProjectKind: ProjectKindHome, ProjectIncluded: true, ProjectExists: true, StartedAt: start, Bytes: 500},
		},
		CursorSubagentsNotImported: 2, CursorDatabaseChecked: true, UnreadableFolders: 2, UnreadableStores: []string{"claude", "codex"},
	}
	for _, reason := range skipOrder {
		harness := "claude"
		if reason == SkipProjectUnknown {
			harness = "cursor"
		}
		p.Candidates = append(p.Candidates, Candidate{Harness: harness, Skip: reason})
	}
	var out bytes.Buffer
	RenderText(&out, p)
	out.WriteString("\n--- cursor database not checked, --include-home set ---\n")
	p.CursorSubagentsNotImported, p.CursorDatabaseChecked, p.CursorDatabaseUnchecked, p.Filters.IncludeHome, p.UnreadableFolders, p.UnreadableStores = 0, false, CursorUncheckedLocked, true, 1, nil
	p.Candidates = p.Candidates[:2]
	RenderText(&out, p)
	golden.Check(t, filepath.Join("testdata", "skip-reasons.txt"), out.Bytes())
	for _, reason := range skipOrder {
		if skipLabels[reason] == "" {
			t.Errorf("%s has no label", reason)
		}
	}
}
