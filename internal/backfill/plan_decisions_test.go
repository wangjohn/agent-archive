package backfill

import (
	"testing"
	"time"
)

func TestSelectAdapterWorkReadsOnlyNeededTranscripts(t *testing.T) {
	t.Parallel()
	workItem := func() *work { return &work{t: &transcript{}} }
	ready := workItem()
	archived := workItem()
	archived.state = SkipAlreadyArchived
	filtered := workItem()
	filtered.filtered = true
	duplicate := workItem()
	duplicate.filtered, duplicate.duplicated = true, true
	unknownProject := workItem()
	unknownProject.res.skip = SkipProjectUnknown
	tooLarge := workItem()
	tooLarge.tooLarge, tooLarge.duplicated = true, true
	items := []*work{ready, archived, filtered, duplicate, unknownProject, tooLarge}

	got := selectAdapterWork(items, time.Time{}, time.Time{})
	if len(got) != 2 || got[0] != ready || got[1] != duplicate {
		t.Fatalf("adapter selection = %p, want ready and duplicate", got)
	}
	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	got = selectAdapterWork(items, since, time.Time{})
	if len(got) != 3 || got[0] != ready || got[1] != duplicate || got[2] != unknownProject {
		t.Fatalf("dated adapter selection = %p, want ready, duplicate, unknown project", got)
	}
}
