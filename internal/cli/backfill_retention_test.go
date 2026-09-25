package cli

import (
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
)

// Regression: 2026-09 review B-21. The import prompt's edit used to accept
// any retention and write it archive-wide: edit, 7, y left the archive at
// 7 days, and the next pass deleted every older session, hook-captured ones
// too. It only raises now; a lower answer keeps the retention and says to
// shorten it in setup.
func TestBackfillEditNeverShortensRetention(t *testing.T) {
	f, _ := newImportFixture(t)
	backdateTranscripts(t, f)
	out, errOut, code := f.importRun(t, strings.NewReader("edit\n7\ny\n"), true, "--background")
	if code != 0 {
		t.Fatalf("code %d: %s", code, errOut)
	}
	if !strings.Contains(out, "Retention stays at 90 days: a shorter period would delete sessions") {
		t.Fatalf("no refusal:\n%s", out)
	}
	cfg, _, err := config.Load(f.data)
	if err != nil || cfg.RetentionDays != 90 {
		t.Fatalf("retention %d, %v", cfg.RetentionDays, err)
	}
	if batch, _ := loadBatch(t, f.data); batch.Retention != nil {
		t.Fatalf("batch records a retention change: %+v", batch.Retention)
	}
}

// B-21: with retention off, edit has nothing to keep longer and turns
// nothing on.
func TestBackfillEditWithRetentionOff(t *testing.T) {
	f, _ := newImportFixture(t)
	cfg, _, err := config.Load(f.data)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RetentionDays = 0
	if err := config.Save(f.data, cfg); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := f.importRun(t, strings.NewReader("edit\ny\n"), true, "--background")
	if code != 0 || !strings.Contains(out, "Retention is off, so no session is deleted") {
		t.Fatalf("code %d\n%s\n%s", code, out, errOut)
	}
	if cfg, _, _ = config.Load(f.data); cfg.RetentionDays != 0 {
		t.Fatalf("retention turned on: %d", cfg.RetentionDays)
	}
}

// B-21: the batch records the retention an edit raised from, and undo puts
// it back, saying so first; a later undo never changes it again.
func TestBackfillUndoRestoresRaisedRetention(t *testing.T) {
	f, _ := newImportFixture(t)
	backdateTranscripts(t, f)
	if _, errOut, code := f.importRun(t, strings.NewReader("edit\n365\ny\n"), true, "--background"); code != 0 {
		t.Fatalf("import: %d %s", code, errOut)
	}
	batch, _ := loadBatch(t, f.data)
	if batch.Retention == nil || batch.Retention.From != 90 || batch.Retention.To != 365 {
		t.Fatalf("batch retention %+v", batch.Retention)
	}
	out, errOut, code := f.undoRun(t, nil, false, "--yes")
	if code != 0 {
		t.Fatalf("undo: %d %s", code, errOut)
	}
	if !strings.Contains(out, "Retention goes back from 365 to 90 days") || !strings.Contains(out, "restored retention to 90 days") {
		t.Fatalf("undo output:\n%s", out)
	}
	cfg, _, _ := config.Load(f.data)
	if cfg.RetentionDays != 90 {
		t.Fatalf("retention after undo %d", cfg.RetentionDays)
	}
	if batch, _ = loadBatch(t, f.data); batch.Retention == nil || !batch.Retention.Restored {
		t.Fatalf("batch retention %+v", batch.Retention)
	}
}

// B-21: retention changed in setup after the import is the person's; undo
// leaves it and says so.
func TestBackfillUndoKeepsRetentionChangedSince(t *testing.T) {
	f, _ := newImportFixture(t)
	backdateTranscripts(t, f)
	if _, errOut, code := f.importRun(t, strings.NewReader("edit\n365\ny\n"), true, "--background"); code != 0 {
		t.Fatalf("import: %d %s", code, errOut)
	}
	cfg, _, _ := config.Load(f.data)
	cfg.RetentionDays = 200
	if err := config.Save(f.data, cfg); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := f.undoRun(t, nil, false, "--yes")
	if code != 0 || !strings.Contains(out, "Retention stays at 200 days") {
		t.Fatalf("undo: %d\n%s\n%s", code, out, errOut)
	}
	if cfg, _, _ = config.Load(f.data); cfg.RetentionDays != 200 {
		t.Fatalf("retention %d", cfg.RetentionDays)
	}
}

// Regression (PR #53 review): an undo that stopped after saving the
// configuration, before recording in the batch what it excluded and
// restored, left no record of either. Its rerun called the restored
// retention "changed after the import", and once setup included the
// projects and raised retention again, the next undo excluded them and
// shortened retention a second time. The rerun now records both.
func TestBackfillUndoRecordsWhatAnInterruptedRunChanged(t *testing.T) {
	f, _ := newImportFixture(t)
	backdateTranscripts(t, f)
	if _, errOut, code := f.importRun(t, strings.NewReader("edit\n365\ny\n"), true, "--background"); code != 0 {
		t.Fatalf("import: %d %s", code, errOut)
	}
	if _, errOut, code := f.undoRun(t, nil, false, "--yes"); code != 0 {
		t.Fatalf("undo: %d %s", code, errOut)
	}
	// The run stopped before recording its configuration changes.
	batch, _ := loadBatch(t, f.data)
	if len(batch.ProjectsExcluded) == 0 || batch.Retention == nil {
		t.Fatalf("batch %+v", batch)
	}
	batch.ProjectsExcluded, batch.Retention.Restored = nil, false
	if err := backfill.SaveBatch(f.data, batch); err != nil {
		t.Fatal(err)
	}

	out, errOut, code := f.undoRun(t, nil, false, "--yes")
	if code != 0 || strings.Contains(out, "Retention stays at") {
		t.Fatalf("rerun: %d\n%s\n%s", code, out, errOut)
	}
	if batch, _ = loadBatch(t, f.data); len(batch.ProjectsExcluded) == 0 || !batch.Retention.Restored {
		t.Fatalf("the rerun did not record the changes: %+v", batch)
	}

	// Setup includes the projects again and raises retention back.
	cfg, _, _ := config.Load(f.data)
	for i := range cfg.Archive.Projects {
		cfg.Archive.Projects[i].Included = true
	}
	cfg.RetentionDays = 365
	if err := config.Save(f.data, cfg); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code = f.undoRun(t, nil, false, "--yes"); code != 0 {
		t.Fatalf("later undo: %d %s", code, errOut)
	}
	after, _, _ := config.Load(f.data)
	if after.RetentionDays != 365 || includedProjects(after.Archive.Projects) != len(after.Archive.Projects) {
		t.Fatalf("a later undo changed setup's choices again: retention %d, projects %+v", after.RetentionDays, after.Archive.Projects)
	}
}
