package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/state"
)

// Regression: 2026-09 review B-23. A batch file without an ID (hand-edited,
// truncated to {}, or written by a version that renamed the field) used to
// select every registration without an import ID, the hook-captured ones,
// into its undo; only SaveBatch's empty-ID check stopped the removal. Undo
// now names the file and changes nothing, and never plans the hook session.
func TestBackfillUndoRefusesBatchWithoutID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		file string
	}{
		{"empty-object", `{}`},
		{"future-start", `{"started_at":"2027-01-01T00:00:00Z"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, bucket := newUndoFixture(t)
			store, err := state.Open(f.data)
			if err != nil {
				t.Fatal(err)
			}
			id, _, err := store.ArchiveSessionID("c-archived")
			if err != nil {
				t.Fatal(err)
			}
			key := "sessions/claude/" + id + "/metadata.json"
			if err := bucket.Put(context.Background(), key, []byte("{}")); err != nil {
				t.Fatal(err)
			}
			// Only the malformed file is left in imports/.
			entries, err := os.ReadDir(filepath.Join(f.data, "imports"))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if err := os.Rename(filepath.Join(f.data, "imports", e.Name()), filepath.Join(f.data, e.Name()+".aside")); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(f.data, "imports", "zz.json"), []byte(tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			before := snapshotTree(t, f.data)
			out, errOut, code := f.undoRun(t, nil, false, "--yes")
			if code != 1 || !strings.Contains(errOut, `"zz"`) || !strings.Contains(errOut, "no valid import ID") || !strings.Contains(errOut, "Nothing was changed") {
				t.Fatalf("code %d\nstdout %s\nstderr %s", code, out, errOut)
			}
			if strings.Contains(out, "If you continue") {
				t.Fatalf("a plan was shown for a batch without an ID:\n%s", out)
			}
			if deletes := bucket.sessionDeletes(); len(deletes) != 0 {
				t.Fatalf("deleted %v", deletes)
			}
			if _, found, err := store.LoadRegistration(id); err != nil || !found {
				t.Fatalf("the hook-captured session was forgotten: %v %v", found, err)
			}
			if after := snapshotTree(t, f.data); after != before {
				t.Fatalf("local state changed:\n%s\n---\n%s", before, after)
			}
		})
	}
}
