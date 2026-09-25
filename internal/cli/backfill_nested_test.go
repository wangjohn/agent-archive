package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

// hookStart runs a Claude Code SessionStart hook for a new session in cwd an
// hour after the fixture's clock and reports whether it was registered.
func (f *backfillFixture) hookStart(t *testing.T, native, cwd string) bool {
	t.Helper()
	env := f.env
	env.Now = func() time.Time { return backfillNow.Add(time.Hour) }
	payload := `{"hook_event_name":"SessionStart","session_id":"` + native + `","cwd":"` + cwd + `","transcript_path":"` + filepath.Join(f.userHome, ".claude", "projects", "x", native+".jsonl") + `"}`
	var errOut bytes.Buffer
	runHookCommand([]string{"--harness", "claude"}, strings.NewReader(payload), &errOut, env)
	_, found, err := state.OpenReadOnly(f.data).ArchiveSessionID(native)
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// Regression: 2026-09 review B-20. The fixture has one session run in
// ~/old-notes, a plain folder. Importing it used to add ~/old-notes as a
// project with no warning, after which a new session in any unconfigured
// repository under it (~/old-notes/secret-repo) was captured. The plan now
// says the folder captures its subfolders and names the repository, which
// the import adds excluded, so a hook there still captures nothing; a plain
// subfolder is captured, as the plan says. Undo removes the entry again.
func TestBackfillKeepsReposUnderAnAddedFolderOutOfCapture(t *testing.T) {
	f, _ := newImportFixture(t)
	notes := filepath.Join(f.userHome, "old-notes")
	secret := filepath.Join(notes, "secret-repo")
	plain := filepath.Join(notes, "drafts")
	for _, dir := range []string{filepath.Join(secret, ".git"), plain} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	out, errOut, code := f.importRun(t, nil, false, "--yes", "--background")
	if code != 0 {
		t.Fatalf("code %d: %s", code, errOut)
	}
	for _, want := range []string{
		"will be added\n  Every future session in a folder under it that isn't in a nearer project",
		"One folder inside it, a repository or app folder, is added as an excluded",
		"    ~/old-notes/secret-repo\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan lacks %q:\n%s", want, out)
		}
	}
	cfg, _, err := config.Load(f.data)
	if err != nil {
		t.Fatal(err)
	}
	if project, owned := configuredProjectActivationFor(cfg, secret); !owned || project.Root != secret || project.Included {
		t.Fatalf("hook owner for the nested repository: %+v %v", project, owned)
	}
	if f.hookStart(t, "new-secret-session", secret) {
		t.Fatal("a new session in the nested repository was captured")
	}
	if !f.hookStart(t, "new-drafts-session", plain) {
		t.Fatal("a new session in a plain subfolder was not captured, as the plan said it would be")
	}
	batch, _ := loadBatch(t, f.data)
	if len(batch.ProjectsKeptOut) != 1 {
		t.Fatalf("batch kept out %v", batch.ProjectsKeptOut)
	}

	out, errOut, code = f.undoRun(t, nil, false, "--yes")
	if code != 0 || !strings.Contains(out, "1 project the import added as excluded") {
		t.Fatalf("undo: %d\n%s\n%s", code, out, errOut)
	}
	if cfg, _, _ = config.Load(f.data); len(cfg.Archive.Projects) == 0 {
		t.Fatal("no projects")
	}
	for _, p := range cfg.Archive.Projects {
		if p.Root == secret {
			t.Fatalf("kept-out entry left after undo: %+v", p)
		}
		if p.Root == notes && p.Included {
			t.Fatalf("the folder is still included: %+v", p)
		}
	}
}
