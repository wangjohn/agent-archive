package cli

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Regression: 2026-09 review B-25. A relative --since (30d) was turned into
// a calendar day before batches were compared, so an import interrupted
// today could not be continued "with the same options" tomorrow: the rerun
// started a new import and left the first one partial. The batch now
// records the value as typed, and the same value continues it on any day.
func TestBackfillRelativeSinceContinuesNextDay(t *testing.T) {
	f, _ := newImportFixture(t)
	backfillHoldSteps = 3
	stopped := false
	backfillCheckpoint = func(step string) error {
		if step == "registered" && !stopped {
			stopped = true
			return errors.New("simulated crash")
		}
		return nil
	}
	t.Cleanup(func() { backfillCheckpoint, backfillHoldSteps = nil, 0 })
	if _, _, code := f.importRun(t, nil, false, "--yes", "--background", "--since", "30d"); code != 1 {
		t.Fatalf("first run: code %d", code)
	}
	b, _ := loadBatch(t, f.data)
	if b.CompletedAt != nil || b.Filters.SinceArg != "30d" || b.Filters.Since != "2026-08-24" {
		t.Fatalf("batch %+v", b)
	}

	backfillCheckpoint = nil
	f.env.Now = func() time.Time { return backfillNow.Add(24 * time.Hour) }
	out, errOut, code := f.importRun(t, nil, false, "--yes", "--background", "--since", "30d")
	if code != 0 {
		t.Fatalf("rerun: %d %s", code, errOut)
	}
	if !strings.Contains(out, "as import "+firstImport+".") || strings.Contains(out, "was interrupted") {
		t.Fatalf("the rerun did not continue %s:\n%s", firstImport, out)
	}
	b, _ = loadBatch(t, f.data)
	if b.CompletedAt == nil {
		t.Fatalf("batch %+v", b)
	}
}

// B-25: when the latest import was interrupted and this run's options
// would start another, the plan says so and gives the command that
// finishes the interrupted one.
func TestBackfillPointsAtAnInterruptedImport(t *testing.T) {
	f, _ := newImportFixture(t)
	stopped := false
	backfillCheckpoint = func(step string) error {
		if step == "registered" && !stopped {
			stopped = true
			return errors.New("simulated crash")
		}
		return nil
	}
	backfillHoldSteps = 3
	t.Cleanup(func() { backfillCheckpoint, backfillHoldSteps = nil, 0 })
	if _, _, code := f.importRun(t, nil, false, "--yes", "--background", "--since", "30d", "--harness", "claude", "--project", f.userHome+"/levenshtein"); code != 1 {
		t.Fatalf("first run: code %d", code)
	}
	backfillCheckpoint = nil
	out, errOut, code := f.command(t, "backfill", "--dry-run")
	if code != 0 {
		t.Fatalf("dry run: %d %s", code, errOut)
	}
	want := "Import " + firstImport + " was interrupted; this run, with other options, starts a new\nimport. To finish " + firstImport + " instead, run:\n  agent-archive backfill --harness claude --project ~/levenshtein --since 30d\n"
	if !strings.Contains(out, want) {
		t.Fatalf("plan lacks\n%s\n---\n%s", want, out)
	}
}
