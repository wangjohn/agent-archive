package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
)

// convergenceState is what an import or undo leaves that must not depend on
// where an earlier run of it stopped: the configured projects (root and
// whether included), retention, the batch's configuration records, and how
// many of the import's sessions are registered.
func convergenceState(t *testing.T, f *backfillFixture) string {
	t.Helper()
	cfg, _, err := config.Load(f.data)
	if err != nil {
		t.Fatal(err)
	}
	var projects []string
	for _, p := range cfg.Archive.Projects {
		projects = append(projects, fmt.Sprintf("%s=%v", strings.TrimPrefix(p.Root, f.userHome), p.Included))
	}
	slices.Sort(projects)
	b, _ := loadBatch(t, f.data)
	parents, children := importRegistrations(t, f.data, firstImport)

	return fmt.Sprintf("projects %v\nretention %d, imported apps %v\nadded %d kept out %d excluded %d kept %d\nretention change %+v completed %v undone %v\nregistered %d+%d, batch lists %d+%d",
		projects, cfg.RetentionDays, cfg.ImportedHarnesses,
		len(b.ProjectsAdded), len(b.ProjectsKeptOut), len(b.ProjectsExcluded), len(b.ProjectsKept),
		b.Retention, b.CompletedAt != nil, b.UndoneAt != nil,
		len(parents), len(children), len(b.Sessions), len(b.Subagents))
}

// newCrashFixture is the import fixture with a repository inside the plain
// folder ~/old-notes, so the import keeps it out.
func newCrashFixture(t *testing.T) *backfillFixture {
	t.Helper()
	f, _ := newImportFixture(t)
	backdateTranscripts(t, f)
	if err := os.MkdirAll(filepath.Join(f.userHome, "old-notes", "secret-repo", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

// importRaising runs an interactive import that raises retention to 365
// days (or only confirms, when it is 365 already, as after a crash past the
// commit).
func importRaising(t *testing.T, f *backfillFixture) (string, string, int) {
	t.Helper()
	answers := "edit\n365\ny\n"
	if cfg, _, _ := config.Load(f.data); cfg.RetentionDays == 365 {
		answers = "y\n"
	}
	return f.importRun(t, strings.NewReader(answers), true, "--background")
}

// PR #53 second review: an import that raises retention and keeps a nested
// repository out, stopped at any step and run again, and then its undo,
// stopped at any step and run again, end exactly where uninterrupted runs
// end: the same projects, kept-out entries, retention, and batch records.
func TestBackfillImportAndUndoConvergeAfterACrashAtEachStep(t *testing.T) {
	t.Parallel()
	clean := newCrashFixture(t)
	if _, errOut, code := importRaising(t, clean); code != 0 {
		t.Fatalf("clean import: %d %s", code, errOut)
	}
	importedWant := convergenceState(t, clean)
	if !strings.Contains(importedWant, "retention 365") || !strings.Contains(importedWant, "/old-notes/secret-repo=false") {
		t.Fatalf("clean import state:\n%s", importedWant)
	}
	if _, errOut, code := clean.undoRun(t, nil, false, "--yes", "--restore-retention"); code != 0 {
		t.Fatalf("clean undo: %d %s", code, errOut)
	}
	undoneWant := convergenceState(t, clean)
	if !strings.Contains(undoneWant, "retention 90") || strings.Contains(undoneWant, "secret-repo") || !strings.Contains(undoneWant, "registered 0+0") {
		t.Fatalf("clean undo state:\n%s", undoneWant)
	}

	crashAt := func(f *backfillFixture, step string) {
		f.env.backfillHoldSteps = 3
		fired := false
		f.env.backfillCheckpoint = func(s string) error {
			if s == step && !fired {
				fired = true
				return errors.New("simulated crash")
			}
			return nil
		}
	}

	for _, step := range []string{"batch saved", "committed", "registered"} {
		t.Run("import "+step, func(t *testing.T) {
			t.Parallel()
			f := newCrashFixture(t)
			crashAt(f, step)
			if _, errOut, code := importRaising(t, f); code != 1 || !strings.Contains(errOut, "simulated crash") {
				t.Fatalf("crash: %d %s", code, errOut)
			}
			f.env.backfillCheckpoint = nil
			if out, errOut, code := importRaising(t, f); code != 0 {
				t.Fatalf("rerun: %d %s\n%s", code, errOut, out)
			}
			if got := convergenceState(t, f); got != importedWant {
				t.Fatalf("after the rerun:\n%s\nwant\n%s", got, importedWant)
			}
		})
	}
	for _, step := range []string{"undoing", "undo marked", "undo configured", "undo recorded"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			f := newCrashFixture(t)
			f.env.backfillCheckpoint = nil
			if _, errOut, code := importRaising(t, f); code != 0 {
				t.Fatalf("import: %d %s", code, errOut)
			}
			crashAt(f, step)
			if _, errOut, code := f.undoRun(t, nil, false, "--yes", "--restore-retention"); code != 1 || !strings.Contains(errOut, "simulated crash") {
				t.Fatalf("crash: %d %s", code, errOut)
			}
			f.env.backfillCheckpoint = nil
			if out, errOut, code := f.undoRun(t, nil, false, "--yes", "--restore-retention"); code != 0 {
				t.Fatalf("rerun: %d %s\n%s", code, errOut, out)
			}
			if got := convergenceState(t, f); got != undoneWant {
				t.Fatalf("after the rerun:\n%s\nwant\n%s", got, undoneWant)
			}
		})
	}
}
