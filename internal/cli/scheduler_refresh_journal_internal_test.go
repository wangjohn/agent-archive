package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// The journal a refresh writes is a files-only one, which neither the commit
// nor a rollback asks launchd anything for, unless its plist changes for a
// job that is loaded: then it is an ordinary one, with was_loaded set, that
// stops the job before the files change and loads it again after. Its
// changes are in the order hook files (Codex, then Claude Code), skills,
// plist, settings.
//
// The golden is the journal setup-transaction.json holds, as text: each
// change's bytes decoded and the paths as tokens. A skill file's text, and
// the settings other than the executable's path, are left out; the skills
// have goldens of their own. The keys of the JSON itself are pinned in
// internal/setupjournal (TestJournalFormatIsPinned).
func TestRefreshJournalIsFilesOnlyUnlessALoadedJobRestarts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   launchdMode
		golden string
	}{
		{"job not loaded", modeMissing, "journal-files-only.txt"},
		{"job loaded", modeLoaded, "journal-restart.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefreshInstall(t, false, true, tc.mode)
			plan, err := planSetupRefresh(r.home, r.userHome, r.newExe, mustLoadConfig(t, r.home), r.env)
			must(t, err)
			// As Commit records it.
			path := filepath.Join(t.TempDir(), "setup-transaction.json")
			must(t, local.Write(path, plan.journal))
			raw := readText(t, path)
			if got := strings.Contains(raw, `"files_only": true`); got != (tc.mode == modeMissing) {
				t.Errorf("files_only recorded: %v\n%s", got, raw)
			}
			if !strings.Contains(raw, fmt.Sprintf("\"was_loaded\": %v", tc.mode == modeLoaded)) {
				t.Errorf("was_loaded:\n%s", raw)
			}
			if plan.journal.Plist != r.plist || plan.journal.Legacy != nil || plan.journal.Relabeled != nil || plan.journal.MoreRelabeled != nil {
				t.Errorf("journal: %+v", plan.journal)
			}
			var view strings.Builder
			fmt.Fprintf(&view, "files_only: %v\nwas_loaded: %v\nplist: %s\n", plan.journal.FilesOnly, plan.journal.WasLoaded, r.tokenize(plan.journal.Plist))
			for _, c := range plan.journal.Changes {
				before, after := string(c.Before), string(c.After)
				switch filepath.Base(c.Path) {
				case "SKILL.md":
					before, after = "(skill file)\n", "(skill file)\n"
				case "config.json":
					before, after = executableOf(t, before), executableOf(t, after)
				}
				fmt.Fprintf(&view, "\n== %s (existed %v, mode %04o, delete %v)\n-- before\n%s-- after\n%s", r.tokenize(c.Path), c.Existed, c.Mode, c.Delete, r.tokenize(before), r.tokenize(after))
			}
			golden.Check(t, filepath.Join("testdata", "scheduler-refresh", tc.golden), []byte(view.String()))
			if setupjournal.TransactionPending(r.home) {
				t.Error("planning wrote a journal")
			}
		})
	}
}

// executableOf is the installed_executable a settings file records, as a line.
func executableOf(t *testing.T, config string) string {
	t.Helper()
	var settings struct {
		InstalledExecutable string `json:"installed_executable"`
	}
	must(t, json.Unmarshal([]byte(config), &settings))
	return "installed_executable: " + settings.InstalledExecutable + "\n"
}
