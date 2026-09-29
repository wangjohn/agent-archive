package cli

import (
	"testing"

	"github.com/wangjohn/agent-archive/internal/backfill"
)

func TestChooseBackfillPlanAction(t *testing.T) {
	t.Parallel()
	empty := backfill.Plan{}
	importable := backfill.Plan{Candidates: []backfill.Candidate{{Harness: "codex"}}}
	for _, tc := range []struct {
		name string
		opts backfillCommandOptions
		plan backfill.Plan
		want backfillPlanAction
	}{
		{"json", backfillCommandOptions{jsonOut: true, dryRun: true}, importable, backfillPlanJSON},
		{"dry run", backfillCommandOptions{dryRun: true}, importable, backfillPlanDryRun},
		{"empty import", backfillCommandOptions{}, empty, backfillPlanEmptyImport},
		{"import", backfillCommandOptions{}, importable, backfillPlanImport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := chooseBackfillPlanAction(tc.opts, tc.plan); got != tc.want {
				t.Fatalf("action = %d, want %d", got, tc.want)
			}
		})
	}
}
