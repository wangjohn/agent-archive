package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
)

// B-2: import A adds levenshtein, and import B later imports another
// session there. Undoing A keeps the project included, says why, and records
// it; undoing B then excludes it. Excluding it with A would have stopped B's
// session updating without B being undone.
func TestBackfillUndoKeepsProjectAnotherImportNeeds(t *testing.T) {
	f, _ := newUndoFixture(t)
	levenshtein := filepath.Join(f.userHome, "levenshtein")
	f.write(t, filepath.Join(".claude", "projects", "slug-c-lev-4", "c-lev-4.jsonl"),
		`{"type":"user","uuid":"a","sessionId":"c-lev-4","cwd":"`+levenshtein+`","timestamp":"2026-09-22T09:00:00Z","message":{"role":"user","content":"again"}}`+"\n")
	backdateTranscripts(t, f)
	if _, errOut, code := f.importRun(t, nil, false, "--yes", "--project", levenshtein); code != 0 {
		t.Fatalf("second import: %d %s", code, errOut)
	}
	const secondImport = "2026-09-23-2"
	if parents, _ := importRegistrations(t, f.data, secondImport); len(parents) != 1 {
		t.Fatalf("second import registered %d sessions", len(parents))
	}

	out, errOut, code := f.undoRun(t, nil, false, firstImport, "--yes")
	if code != 0 {
		t.Fatalf("undo A: %d %s\n%s", code, errOut, out)
	}
	for _, want := range []string{"1 project the import added stays included", "~/levenshtein (1 session from import " + secondImport + ")"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan lacks %q:\n%s", want, out)
		}
	}
	if !included(t, f.data, levenshtein) {
		t.Fatal("undoing A excluded a project B still has sessions in")
	}
	batches, err := backfill.LoadBatches(f.data)
	if err != nil {
		t.Fatal(err)
	}
	if kept := batches[0].ProjectsKept; batches[0].ID != firstImport || len(kept) != 1 {
		t.Fatalf("import A recorded kept %v", kept)
	}
	if line := historyLine(t, f, firstImport); strings.Contains(line, "project left") {
		t.Fatalf("a project kept for B counted as A's: %s", line)
	}

	out, errOut, code = f.undoRun(t, nil, false, secondImport, "--yes")
	if code != 0 || !strings.Contains(out, "excluded from capture") {
		t.Fatalf("undo B: %d %s\n%s", code, errOut, out)
	}
	if included(t, f.data, levenshtein) {
		t.Fatal("undoing the last import with sessions there left the project included")
	}
}

func included(t *testing.T, home, root string) bool {
	t.Helper()
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range cfg.Archive.Projects {
		if p.Root == root {
			return p.Included
		}
	}
	return false
}

// B-1: the error an unreadable import file gives no longer suggests moving
// it aside as if that were harmless.
func TestBackfillUnreadableImportFileAdvice(t *testing.T) {
	f, _ := newUndoFixture(t)
	f.write(t, filepath.Join("..", "data", "imports", "2026-09-22-1.json"), "{not json")
	f.write(t, filepath.Join(".claude", "projects", "slug-c-new", "c-new.jsonl"),
		`{"type":"user","uuid":"a","sessionId":"c-new","cwd":"`+filepath.Join(f.userHome, "levenshtein")+`","timestamp":"2026-09-22T10:00:00Z","message":{"role":"user","content":"hi"}}`+"\n")
	backdateTranscripts(t, f)
	_, errOut, code := f.importRun(t, nil, false, "--yes")
	if code == 0 || !strings.Contains(errOut, "backfill undo can no longer remove them") {
		t.Fatalf("code %d: %s", code, errOut)
	}
}
