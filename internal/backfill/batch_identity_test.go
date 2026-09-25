package backfill

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Regression: 2026-09 review B-23. A batch file whose ID is missing,
// malformed, or not its file's name, or that has no start, is not a batch:
// LoadBatches names it as unreadable instead of returning it, so undo (which
// must see every batch) stops rather than select sessions by an empty ID.
func TestLoadBatchesRejectsInvalidBatchFiles(t *testing.T) {
	for _, tc := range []struct{ name, file, body, want string }{
		{"empty object", "zz", `{}`, "no valid import ID"},
		{"empty id", "2026-09-23-7", `{"id":"","started_at":"2026-09-23T10:00:00Z"}`, "no valid import ID"},
		{"future start only", "zz", `{"started_at":"2027-01-01T00:00:00Z"}`, "no valid import ID"},
		{"path in id", "x", `{"id":"../2026-09-23-1","started_at":"2026-09-23T10:00:00Z"}`, "no valid import ID"},
		{"id of another file", "2026-09-23-2", `{"id":"2026-09-23-1","started_at":"2026-09-23T10:00:00Z"}`, "does not match its file name"},
		{"no start", "2026-09-23-3", `{"id":"2026-09-23-3"}`, "no start time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUndoFixture(t)
			good := f.batch("2026-09-22-1", fixedNow.Add(-24*time.Hour))
			if err := os.WriteFile(filepath.Join(f.home, "imports", tc.file+".json"), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			batches, err := LoadBatches(f.home)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `"`+tc.file+`"`) {
				t.Fatalf("err %v, want it to name %q and say %q", err, tc.file, tc.want)
			}
			if len(batches) != 1 || batches[0].ID != good.ID {
				t.Fatalf("batches %+v, want only the valid one", batches)
			}
		})
	}
}

func TestValidBatchID(t *testing.T) {
	for id, want := range map[string]bool{
		"2026-09-23-1": true, "2026-09-23-12": true,
		"": false, "2026-09-23": false, "2026-09-23-0": false, "2026-09-23-01": false,
		"../2026-09-23-1": false, "2026-09-23-1/x": false, "2026-09-23-1 ": false, "x2026-09-23-1": false,
	} {
		if got := ValidBatchID(id); got != want {
			t.Errorf("ValidBatchID(%q) = %v, want %v", id, got, want)
		}
	}
}

// Regression: 2026-09 review B-23. PlanUndo refuses a batch without a valid
// ID outright, whatever the registrations carry.
func TestPlanUndoRefusesBatchWithoutID(t *testing.T) {
	f := newUndoFixture(t)
	f.include("/p")
	f.register("hook-session", "/p", "", fixedNow.UTC())
	for _, id := range []string{"", "not-an-id"} {
		b := Batch{ID: id, StartedAt: fixedNow.Add(-time.Hour)}
		if _, err := PlanUndo(Environment{Home: f.home}, f.store, f.cfg, []Batch{b}, b, ""); err == nil {
			t.Fatalf("ID %q: planned an undo", id)
		}
	}
}

// Invariant (B-23 hardening): whatever registrations and batches exist,
// undo only ever selects registrations InBatch proves are the batch's: an
// import registration carrying its ID. Hook registrations, including one
// carrying an import ID, and other imports' sessions are never selected.
func TestPlanUndoSelectsOnlyProvablyImportedRegistrations(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	ids := []string{"2026-09-23-1", "2026-09-23-2", "2026-09-22-1"}
	for round := range 20 {
		f := newUndoFixture(t)
		f.include("/p")
		f.include("/q")
		for i := range 12 {
			batch := ""
			if rng.IntN(3) > 0 {
				batch = ids[rng.IntN(len(ids))]
			}
			origin := archive.SessionOriginHook
			if rng.IntN(2) == 0 {
				origin = archive.SessionOriginImport
			}
			root := []string{"/p", "/q"}[rng.IntN(2)]
			native := "s-" + string(rune('a'+round)) + "-" + string(rune('a'+i))
			if _, err := f.store.RegisterNewSession(native, func(id string) archive.SessionRegistration {
				return archive.SessionRegistration{
					ArchiveSessionID: id, NativeSessionID: native, ProjectID: archive.ProjectID(root), ProjectRoot: root,
					Harness: archive.Harness{Name: "claude"}, SessionStartedAt: fixedNow.Add(-2 * time.Hour),
					RegisteredAt: fixedNow, AdmittedAt: fixedNow, Origin: origin, ImportBatch: batch, DestinationID: "dest",
				}
			}); err != nil {
				t.Fatal(err)
			}
		}
		var batches []Batch
		for _, id := range ids {
			b := Batch{ID: id, StartedAt: fixedNow.Add(-time.Hour), DestinationID: "dest"}
			batches = append(batches, b)
		}
		for _, b := range batches {
			plan, err := PlanUndo(Environment{Home: f.home}, f.store, f.cfg, batches, b, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range plan.Sessions {
				if s.Registration.Origin != archive.SessionOriginImport || s.Registration.ImportBatch != b.ID {
					t.Fatalf("round %d: undo of %s selected %+v", round, b.ID, s.Registration)
				}
			}
		}
	}
}

// Guard (B-23 hardening): every comparison with a registration's import ID
// in production code goes through InBatch, so no caller can select a
// hook-captured session, or everything with an empty ID, by comparing the
// field itself. Comparisons with "" (is there an ID at all) are allowed.
func TestImportBatchComparedOnlyThroughInBatch(t *testing.T) {
	compare := regexp.MustCompile(`\.ImportBatch\s*(==|!=)\s*[^"\s]|[^"\s]\s*(==|!=)\s*[A-Za-z_.]*\.ImportBatch\b`)
	var offenders []string
	for _, dir := range []string{".", filepath.Join("..", "cli"), filepath.Join("..", "collector"), filepath.Join("..", "state")} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			for n, line := range strings.Split(string(data), "\n") {
				if compare.MatchString(line) && !strings.Contains(line, "reg.Imported() && reg.ImportBatch == id") {
					offenders = append(offenders, file+":"+strconv.Itoa(n+1)+": "+strings.TrimSpace(line))
				}
			}
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("compare import IDs through InBatch:\n%s", strings.Join(offenders, "\n"))
	}
}

// B-25: a relative --since or --until continues a batch when typed alike,
// whatever day it names now; otherwise the days are compared, as for a
// batch that recorded no value.
func TestBatchFiltersMatchRelativeBoundsAsTyped(t *testing.T) {
	for _, tc := range []struct {
		a, b BatchFilters
		want bool
	}{
		{BatchFilters{Since: "2026-08-24", SinceArg: "30d"}, BatchFilters{Since: "2026-08-25", SinceArg: "30d"}, true},
		{BatchFilters{Since: "2026-08-24", SinceArg: "30d"}, BatchFilters{Since: "2026-08-24", SinceArg: "720h"}, false},
		{BatchFilters{Since: "2026-08-24", SinceArg: "30d"}, BatchFilters{Since: "2026-08-24"}, true},
		{BatchFilters{Since: "2026-08-24"}, BatchFilters{Since: "2026-08-25", SinceArg: "30d"}, false},
		{BatchFilters{Until: "2026-09-01", UntilArg: "3d"}, BatchFilters{Until: "2026-09-02", UntilArg: "3d"}, true},
	} {
		if got := tc.a.equal(tc.b); got != tc.want {
			t.Errorf("%+v vs %+v: %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
	if got := shellWord("~/my code/it's"); got != `'~/my code/it'\''s'` {
		t.Errorf("shellWord: %s", got)
	}
	if got := shellWord("~/code/repo-1"); got != "~/code/repo-1" {
		t.Errorf("shellWord: %s", got)
	}
}
