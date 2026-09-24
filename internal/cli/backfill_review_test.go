package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// An import can add projects whose folders no longer exist. Setup keeps
// working afterwards: only a project the setup newly includes must exist.
func TestSetupAfterImportWithMissingFolders(t *testing.T) {
	f, bucket := newImportFixture(t)
	if _, errOut, code := f.importRun(t, nil, false, "--yes", "--background"); code != 0 {
		t.Fatalf("import: %s", errOut)
	}
	oldNotes := filepath.Join(f.userHome, "old-notes")
	env := setupTestEnv(t, f.data, f.userHome, newFakeKeychain(), backfillNow)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return bucket, nil }
	setupRun(t, env, "retention\n30\ny\n", 0)
	cfg, _, _ := config.Load(f.data)
	if cfg.RetentionDays != 30 {
		t.Fatalf("retention %d", cfg.RetentionDays)
	}
	kept := false
	for _, p := range cfg.Archive.Projects {
		kept = kept || (p.Root == oldNotes && p.Included)
	}
	if !kept {
		t.Fatal("the imported project with a missing folder was dropped")
	}

	// A missing folder the setup newly includes, or includes again after
	// excluding it, is still refused.
	executable := testExecutable(t)
	for _, tc := range []struct {
		name string
		edit func(next *config.Config)
	}{
		{"new", func(next *config.Config) {
			root := filepath.Join(f.userHome, "never-existed")
			next.Archive.Projects = append(next.Archive.Projects, archive.ProjectActivation{ProjectID: archive.ProjectID(root), Root: root, Included: true})
		}},
		{"re-included", func(next *config.Config) {
			for i := range next.Archive.Projects {
				if next.Archive.Projects[i].Root == oldNotes {
					next.Archive.Projects[i].Included = true
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old, _, _ := config.Load(f.data)
			for i := range old.Archive.Projects {
				if old.Archive.Projects[i].Root == oldNotes {
					old.Archive.Projects[i].Included = false
				}
			}
			if err := config.Save(f.data, old); err != nil {
				t.Fatal(err)
			}
			next := old
			next.Archive.Projects = append([]archive.ProjectActivation(nil), old.Archive.Projects...)
			tc.edit(&next)
			err := applySetup(f.data, f.userHome, executable, old, &next, nil, env)
			if err == nil || !strings.Contains(err.Error(), "no longer a directory") {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// The projects backfill added are kept or dropped with one question; the
// others are still asked about one by one.
func TestSetupGroupsBackfilledProjects(t *testing.T) {
	existing := []archive.ProjectActivation{
		{ProjectID: "p-mine", Root: "/work/mine", Included: true},
		{ProjectID: "p-1", Root: "/work/imported-1", Included: true},
		{ProjectID: "p-2", Root: "/work/imported-2", Included: true},
		{ProjectID: "p-3", Root: "/work/imported-3", Included: true},
	}
	backfilled := map[string]bool{"p-1": true, "p-2": true, "p-3": true}
	for _, tc := range []struct {
		input string
		want  int
	}{{"y\ny\n\n", 4}, {"n\ny\n\n", 1}} {
		var out bytes.Buffer
		projects, err := promptProjects(newPrompter(strings.NewReader(tc.input), &out), existing, backfilled)
		if err != nil {
			t.Fatal(err)
		}
		// Declined imported projects stay, excluded, so the decision holds.
		if len(projects) != 4 || includedProjects(projects) != tc.want {
			t.Fatalf("%q: %+v, want %d included", tc.input, projects, tc.want)
		}
		for _, p := range projects {
			if backfilled[p.ProjectID] && p.Included != (tc.want == 4) {
				t.Errorf("%q: %+v", tc.input, p)
			}
		}
		text := out.String()
		if strings.Count(text, "Keep the 3 projects added by backfill? If not, their imported sessions stop uploading and later backfills skip them. [Y/n]") != 1 ||
			strings.Count(text, "Keep project") != 1 || !strings.Contains(text, "Keep project /work/mine?") {
			t.Fatalf("prompts:\n%s", text)
		}
	}
}

// Exclusions, such as those undo leaves, survive a setup edit of the
// projects, and typing an excluded project's path includes it again.
func TestSetupKeepsExclusions(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	existing := []archive.ProjectActivation{
		{ProjectID: "p-kept", Root: "/work/kept", Included: true},
		{ProjectID: "p-gone", Root: "/work/excluded", Included: false},
		{ProjectID: archive.ProjectID(root), Root: root, Included: false},
	}
	var out bytes.Buffer
	projects, err := promptProjects(newPrompter(strings.NewReader("y\n\n"), &out), existing, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 3 || includedProjects(projects) != 1 || strings.Contains(out.String(), "/work/excluded") {
		t.Fatalf("%+v\n%s", projects, out.String())
	}
	projects, err = promptProjects(newPrompter(strings.NewReader("y\n"+root+"\n\n"), &out), existing, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 3 || includedProjects(projects) != 2 {
		t.Fatalf("re-including: %+v", projects)
	}
}

// A setup draft saved before an import, then continued, keeps the projects
// the import added.
func TestSetupDraftKeepsBackfilledProjects(t *testing.T) {
	f, bucket := newImportFixture(t)
	before, _, _ := config.Load(f.data)
	if err := local.Write(filepath.Join(f.data, "setup-draft.json"), setupDraft{Version: 1, Step: 2, Config: before}); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := f.importRun(t, nil, false, "--yes", "--background"); code != 0 {
		t.Fatalf("import: %s", errOut)
	}
	env := setupTestEnv(t, f.data, f.userHome, newFakeKeychain(), backfillNow)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return bucket, nil }
	setupRun(t, env, "continue\ny\n", 0)
	cfg, _, _ := config.Load(f.data)
	if len(cfg.Archive.Projects) != 5 || includedProjects(cfg.Archive.Projects) != 5 {
		t.Fatalf("projects after continuing the draft: %+v", cfg.Archive.Projects)
	}
}

// A registration error after an earlier run crashed inside a hold still
// leaves the batch with every session registered under it: the error path
// rebuilds the batch from the registrations.
func TestBackfillErrorPathReconciles(t *testing.T) {
	f, _ := newImportFixture(t)
	imports := filepath.Join(f.data, "imports")
	backfillHoldSteps = 3
	backfillCheckpoint = func(step string) error {
		if step == "committed" {
			return os.Chmod(imports, 0o500)
		}
		return nil
	}
	t.Cleanup(func() {
		backfillCheckpoint, backfillHoldSteps = nil, 0
		_ = os.Chmod(imports, 0o700)
	})
	if _, _, code := f.importRun(t, nil, false, "--yes", "--background"); code != 1 {
		t.Fatal("the first run did not fail")
	}
	if err := os.Chmod(imports, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := loadBatch(t, f.data)
	crashed, _ := importRegistrations(t, f.data, firstImport)
	if len(b.Sessions) >= len(crashed) {
		t.Fatalf("no gap to repair: %d recorded, %d registered", len(b.Sessions), len(crashed))
	}

	backfillCheckpoint = func(step string) error {
		if step == "registered" {
			return errors.New("simulated registration error")
		}
		return nil
	}
	if _, errOut, code := f.importRun(t, nil, false, "--yes", "--background"); code != 1 || !strings.Contains(errOut, "simulated registration error") {
		t.Fatalf("second run: code %d, %s", code, errOut)
	}
	registered, _ := importRegistrations(t, f.data, firstImport)
	b, _ = loadBatch(t, f.data)
	if len(registered) <= len(crashed) || len(b.Sessions) != len(registered) || b.CompletedAt != nil {
		t.Fatalf("%d registered, batch records %d", len(registered), len(b.Sessions))
	}
}

// A crash inside a registration hold, here the batch file becoming
// unwritable, leaves registrations the batch file never recorded. The rerun
// rebuilds the batch from the registrations.
func TestBackfillBatchRebuiltFromRegistrations(t *testing.T) {
	f, _ := newImportFixture(t)
	imports := filepath.Join(f.data, "imports")
	backfillHoldSteps = 3
	backfillCheckpoint = func(step string) error {
		if step == "committed" {
			return os.Chmod(imports, 0o500)
		}
		return nil
	}
	t.Cleanup(func() {
		backfillCheckpoint, backfillHoldSteps = nil, 0
		_ = os.Chmod(imports, 0o700)
	})
	if _, errOut, code := f.importRun(t, nil, false, "--yes", "--background"); code != 1 {
		t.Fatalf("the unwritable batch file did not stop the import: %s", errOut)
	}
	registered, _ := importRegistrations(t, f.data, firstImport)
	b, _ := loadBatch(t, f.data)
	if len(registered) == 0 || len(b.Sessions) >= len(registered) {
		t.Fatalf("setup failed to reproduce the gap: %d registered, %d recorded", len(registered), len(b.Sessions))
	}

	backfillCheckpoint = nil
	if err := os.Chmod(imports, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, errOut, code := f.importRun(t, nil, false, "--yes", "--background"); code != 0 {
		t.Fatalf("rerun: %s\n%s", errOut, out)
	}
	parents, _ := importRegistrations(t, f.data, firstImport)
	b, _ = loadBatch(t, f.data)
	if len(parents) != 12 || len(b.Sessions) != 12 || len(b.Subagents) != 2 || b.CompletedAt == nil {
		t.Fatalf("%d registered, batch %+v", len(parents), b)
	}
}

// A run that finds nothing left to import completes the interrupted import
// it would have continued, from the registrations.
func TestBackfillCompletesInterruptedImportWithNothingLeft(t *testing.T) {
	f, _ := newImportFixture(t)
	if _, errOut, code := f.importRun(t, nil, false, "--yes", "--background"); code != 0 {
		t.Fatalf("import: %s", errOut)
	}
	b, _ := loadBatch(t, f.data)
	b.CompletedAt, b.Sessions = nil, b.Sessions[:2]
	if err := backfill.SaveBatch(f.data, b); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := f.importRun(t, nil, false, "--yes")
	if code != 0 || !strings.Contains(out, "Nothing to import.") || !strings.Contains(out, "Import "+firstImport+", which was interrupted, is complete: 12 sessions registered.") {
		t.Fatalf("code %d, %s\n%s", code, errOut, out)
	}
	b, _ = loadBatch(t, f.data)
	if b.CompletedAt == nil || len(b.Sessions) != 12 {
		t.Fatalf("batch %+v", b)
	}
	// Other filters would not have continued it, so they leave it alone.
	b.CompletedAt = nil
	if err := backfill.SaveBatch(f.data, b); err != nil {
		t.Fatal(err)
	}
	if out, _, _ := f.importRun(t, nil, false, "--yes", "--harness", "claude"); strings.Contains(out, "is complete") {
		t.Fatalf("a run with other filters completed the import:\n%s", out)
	}
}

// Ctrl-C during registration stops between holds and leaves an interrupted
// import that a rerun finishes.
func TestBackfillInterruptedRegistration(t *testing.T) {
	f, _ := newImportFixture(t)
	signals := make(chan os.Signal, 1)
	f.env.Interrupts = func() (<-chan os.Signal, func()) { return signals, func() {} }
	backfillHoldSteps = 3
	backfillCheckpoint = func(step string) error {
		if step == "registered" && len(signals) == 0 {
			signals <- os.Interrupt
		}
		return nil
	}
	t.Cleanup(func() { backfillCheckpoint, backfillHoldSteps = nil, 0 })
	out, _, code := f.importRun(t, nil, false, "--yes")
	if code != 1 || !strings.Contains(out, "Stopped.") || !strings.Contains(out, "run agent-archive backfill again with the same options to finish it") {
		t.Fatalf("code %d:\n%s", code, out)
	}
	partial, _ := importRegistrations(t, f.data, firstImport)
	b, _ := loadBatch(t, f.data)
	if b.CompletedAt != nil || len(partial) == 0 || len(partial) == 12 || len(b.Sessions) != len(partial) {
		t.Fatalf("%d registered, batch %+v", len(partial), b)
	}

	backfillCheckpoint = nil
	f.env.Interrupts = nil
	if _, errOut, code := f.importRun(t, nil, false, "--yes", "--background"); code != 0 {
		t.Fatalf("rerun: %s", errOut)
	}
	b, _ = loadBatch(t, f.data)
	if b.CompletedAt == nil || len(b.Sessions) != 12 {
		t.Fatalf("batch %+v", b)
	}
}

// history words each state accurately, and neither history nor status fails
// on an unreadable import file.
func TestBackfillHistoryStates(t *testing.T) {
	f, _ := newImportFixture(t)
	done := backfillNow.UTC()
	for i, b := range []backfill.Batch{
		{ID: "2026-09-20-1"},
		{ID: "2026-09-21-1", CompletedAt: &done},
		{ID: "2026-09-22-1"},
	} {
		b.StartedAt = backfillNow.Add(time.Duration(i-3) * 24 * time.Hour)
		if err := backfill.SaveBatch(f.data, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.data, "imports", "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := f.command(t, "backfill", "history")
	if code != 0 || !strings.Contains(errOut, "broken") {
		t.Fatalf("code %d, %s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 {
		t.Fatalf("history:\n%s", out)
	}
	for i, want := range []string{"interrupted; 0 registered", "nothing registered", "interrupted; 0 registered; run agent-archive backfill with the same options to finish"} {
		line := lines[i+1]
		if !strings.HasSuffix(line, want) || (i == 0 && strings.Contains(line, "run agent-archive")) {
			t.Errorf("line %d = %q, want it to end %q", i+1, line, want)
		}
	}

	out, errOut, code = f.command(t, "status", "--json")
	if code != 0 {
		t.Fatalf("status: %s", errOut)
	}
	var view statusView
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatal(err)
	}
	if view.LastImport != "2026-09-22-1" || !strings.Contains(strings.Join(view.Warnings, "\n"), "broken") {
		t.Fatalf("last import %q, warnings %v", view.LastImport, view.Warnings)
	}
}

// With every import file unreadable, history says so instead of printing an
// empty table, and an import refuses without changing anything.
func TestBackfillAllImportsUnreadable(t *testing.T) {
	f, _ := newImportFixture(t)
	if err := os.MkdirAll(filepath.Join(f.data, "imports"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.data, "imports", "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := f.command(t, "backfill", "history")
	if code != 1 || !strings.Contains(out, "No import could be read") || strings.Contains(out, "IMPORT") || !strings.Contains(errOut, "broken") {
		t.Fatalf("code %d\n%s\n%s", code, out, errOut)
	}
	cfgBefore, _ := os.ReadFile(filepath.Join(f.data, "config.json"))
	_, errOut, code = f.importRun(t, nil, false, "--yes")
	if code != 1 || !strings.Contains(errOut, "Nothing was changed") || !strings.Contains(errOut, "imports") {
		t.Fatalf("code %d, %s", code, errOut)
	}
	if cfgAfter, _ := os.ReadFile(filepath.Join(f.data, "config.json")); !bytes.Equal(cfgBefore, cfgAfter) {
		t.Fatal("config changed")
	}
	if parents, _ := importRegistrations(t, f.data, firstImport); len(parents) != 0 {
		t.Fatal("sessions registered")
	}
}
