package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
)

// asMainBatch rewrites the import's batch file with only the keys main
// wrote before PR #53 (no since_arg, until_arg, retention,
// projects_kept_out, or projects_kept_for), as a batch written by an
// earlier build is on disk.
func asMainBatch(t *testing.T, home string) {
	t.Helper()
	path := filepath.Join(home, "imports", firstImport+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	keep := map[string]bool{"id": true, "started_at": true, "completed_at": true, "filters": true, "destination_id": true,
		"projects_added": true, "apps_added": true, "sessions": true, "subagents": true, "undone_at": true,
		"projects_excluded": true, "projects_kept": true}
	for k := range raw {
		if !keep[k] {
			delete(raw, k)
		}
	}
	var filters map[string]json.RawMessage
	if err := json.Unmarshal(raw["filters"], &filters); err != nil {
		t.Fatal(err)
	}
	keepFilters := map[string]bool{"harnesses": true, "project_ids": true, "since": true, "until": true,
		"include_home": true, "include_temp": true, "include_removed": true}
	for k := range filters {
		if !keepFilters[k] {
			delete(filters, k)
		}
	}
	if raw["filters"], err = json.Marshal(filters); err != nil {
		t.Fatal(err)
	}
	if data, err = json.MarshalIndent(raw, "", "  "); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// PR #53 second review: a batch file written by main before this change
// (an import interrupted with --since 30d) still loads, is continued by the
// same options on the same day, shows in history, and is undone: its
// sessions deleted and the projects it added excluded.
func TestBackfillMainBatchStillContinuesAndUndoes(t *testing.T) {
	t.Parallel()
	f, bucket := newImportFixture(t)
	f.env.backfillHoldSteps = 3
	stopped := false
	f.env.backfillCheckpoint = func(step string) error {
		if step == "registered" && !stopped {
			stopped = true
			return errors.New("simulated crash")
		}
		return nil
	}
	if _, _, code := f.importRun(t, nil, false, "--yes", "--background", "--since", "30d"); code != 1 {
		t.Fatalf("first run: code %d", code)
	}
	asMainBatch(t, f.data)
	if b, raw := loadBatch(t, f.data); b.CompletedAt != nil || b.Filters.SinceArg != "" || strings.Contains(string(raw), "since_arg") {
		t.Fatalf("not a main-era interrupted batch: %s", raw)
	}

	f.env.backfillCheckpoint = nil
	out, errOut, code := f.importRun(t, nil, false, "--yes", "--background", "--since", "30d")
	if code != 0 || !strings.Contains(out, "as import "+firstImport+".") {
		t.Fatalf("rerun did not continue %s: %d %s\n%s", firstImport, code, errOut, out)
	}
	b, _ := loadBatch(t, f.data)
	if b.CompletedAt == nil || len(b.Sessions) == 0 || len(b.ProjectsAdded) == 0 {
		t.Fatalf("batch %+v", b)
	}
	asMainBatch(t, f.data)

	if out, errOut, code = f.command(t, "backfill", "history"); code != 0 || !strings.Contains(out, firstImport) {
		t.Fatalf("history: %d %s\n%s", code, errOut, out)
	}
	if regs, _ := importRegistrations(t, f.data, firstImport); len(regs) == 0 {
		t.Fatal("nothing registered")
	}
	if out, errOut, code = f.undoRun(t, nil, false, "--yes"); code != 0 {
		t.Fatalf("undo: %d %s\n%s", code, errOut, out)
	}
	if regs, subs := importRegistrations(t, f.data, firstImport); len(regs)+len(subs) != 0 {
		t.Fatalf("%d still registered", len(regs)+len(subs))
	}
	if keys := bucketSnapshot(t, bucket); keys != "" {
		t.Fatalf("left in the bucket: %v", keys)
	}
	cfg, _, err := config.Load(f.data)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range cfg.Archive.Projects {
		if p.Included && slices.Contains(b.ProjectsAdded, p.ProjectID) {
			t.Fatalf("added project %s still included", p.Root)
		}
	}
	if b, _ = loadBatch(t, f.data); b.UndoneAt == nil || len(b.ProjectsExcluded) != len(b.ProjectsAdded) {
		t.Fatalf("undo not recorded: %+v", b)
	}
}
