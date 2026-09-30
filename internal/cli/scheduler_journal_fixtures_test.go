package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

// interruptedSetup is what the fake launchctl panics with to end setup where
// a crash would: after every file is changed and the old jobs are stopped, when
// the new job is about to start.
type interruptedSetup struct{}

// crashingSetup runs setup, which ends at the crash; it fails the test when
// setup finishes instead.
func (r *schedRun) crashingSetup(extra ...string) {
	r.t.Helper()
	r.crashAt = r.own()
	defer func() {
		r.t.Helper()
		if v := recover(); v != nil {
			if _, ok := v.(interruptedSetup); !ok {
				panic(v)
			}
			r.crashAt = ""
			return
		}
		r.t.Fatal("setup was not interrupted")
	}()
	r.setup(extra...)
}

// TestWriteInterruptedSetupFixtures writes the journals in
// testdata/scheduler/journals/, and only when it is asked to:
//
//	AGENT_ARCHIVE_WRITE_JOURNAL_FIXTURES=1 go test ./internal/cli -run TestWriteInterruptedSetupFixtures
//
// It runs the real setup on a fake Mac, interrupts it as a crash would (see
// interruptedSetup), takes the journal the code left, and rewrites the paths of
// the fake Mac's temporary folders to the fixed ones the fixtures use
// (rewritePaths). The fixtures are the format releases of today write and
// read: do not regenerate them to make a change pass; add new ones beside them.
func TestWriteInterruptedSetupFixtures(t *testing.T) {
	if os.Getenv("AGENT_ARCHIVE_WRITE_JOURNAL_FIXTURES") == "" {
		t.Skip("writes testdata/scheduler/journals; run it only to make new fixtures")
	}
	for name, interrupt := range map[string]func(r *schedRun){
		// This installation's job loaded from an older executable's plist, and
		// two collectors under earlier labels, one loaded (Relabeled, then
		// MoreRelabeled), all stopped by a setup from a newer executable.
		"resetup-earlier-labels": func(r *schedRun) {
			r.install()
			r.earlierLabel(hooks.CollectorLabel("/old/spelling/a", ""), true)
			r.earlierLabel(hooks.CollectorLabel("/old/spelling/b", ""), false)
			other := testExecutable(t)
			r.env.Executable = func() (string, error) { return other, nil }
			r.exe = other
			r.crashingSetup()
		},
		// A first setup that retires the prototype's running upload job (Legacy).
		"first-setup-prototype": func(r *schedRun) {
			r.loadedPrototype()
			r.crashingSetup()
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newSchedRun(t, true)
			firstExe := r.exe
			interrupt(r)
			raw, err := os.ReadFile(setupjournal.JournalPath(r.home))
			must(t, err)
			pairs := []string{}
			for _, from := range []string{r.userHome, r.account, r.project} {
				to := map[string]string{r.userHome: fixtureUserHome, r.account: fixtureAccount, r.project: fixtureProject}[from]
				pairs = append(pairs, from, to, local.CanonicalPath(from), to)
			}
			pairs = append(pairs, firstExe, fixtureBin)
			if r.exe != firstExe {
				pairs = append(pairs, r.exe, fixtureNewBin)
			}
			out := rewritePaths(t, raw, pairs...)
			transformJournal(t, out, func(text string) string {
				for _, temporary := range []string{os.TempDir(), local.CanonicalPath(os.TempDir())} {
					if strings.Contains(text, temporary) {
						t.Fatalf("the fixture still holds a path of this machine (%s in %q)", temporary, text)
					}
				}
				return text
			})
			must(t, os.MkdirAll(filepath.Dir(fixturePath(name)), 0o755))
			must(t, os.WriteFile(fixturePath(name), out, 0o644))
		})
	}
}
