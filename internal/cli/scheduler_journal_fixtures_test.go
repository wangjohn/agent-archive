package cli

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The journal fixtures (testdata/scheduler/journals; its README says how they
// were made): setup-transaction.json as the code of their commit wrote it, in
// the format every later release must still recover. Here is that format as
// the tests read it, without the code under test's types, and the test that
// writes the fixtures.

// journalFile is setup-transaction.json: every file change setup makes, the
// collector's plist and whether its job was loaded, and the jobs it retires.
type journalFile struct {
	Legacy        *journalJob     `json:"legacy"`
	Relabeled     *journalJob     `json:"relabeled"`
	MoreRelabeled []*journalJob   `json:"more_relabeled"`
	Changes       []journalChange `json:"changes"`
	Plist         string          `json:"plist"`
	WasLoaded     bool            `json:"was_loaded"`
}

// journalJob is a job setup retires; Change.Before is its plist as found.
type journalJob struct {
	Change    journalChange `json:"change"`
	WasLoaded bool          `json:"was_loaded"`
}

// journalChange is one file's change; Before and After are base64 on disk.
type journalChange struct {
	Path    string `json:"Path"`
	Before  []byte `json:"Before"`
	After   []byte `json:"After"`
	Existed bool   `json:"Existed"`
	Delete  bool   `json:"Delete"`
}

// retired lists the jobs the journal retires: the prototype's, then the
// earlier labels'.
func (j journalFile) retired() []*journalJob {
	var jobs []*journalJob
	for _, job := range append([]*journalJob{j.Legacy, j.Relabeled}, j.MoreRelabeled...) {
		if job != nil {
			jobs = append(jobs, job)
		}
	}
	return jobs
}

// The journal fixtures name the paths of the fake Mac they were written on;
// a replay maps them to its own temporary folders.
const (
	fixtureUserHome = "/fixture/user-home"
	fixtureAccount  = "/fixture/account"
	fixtureProject  = "/fixture/project"
	fixtureBin      = "/fixture/bin/agent-archive"
	fixtureNewBin   = "/fixture/bin-new/agent-archive"
)

func fixturePath(name string) string {
	return filepath.Join("testdata", "scheduler", "journals", name+".json")
}

// rewritePaths rewrites the paths in a journal by pairs (old, new, ...).
func rewritePaths(t *testing.T, raw []byte, pairs ...string) []byte {
	t.Helper()
	return transformJournal(t, raw, strings.NewReplacer(pairs...).Replace)
}

// The journal fields that hold a path, and the ones that hold a file's bytes.
var (
	journalPathKeys = map[string]bool{"Path": true, "plist": true}
	journalFileKeys = map[string]bool{"Before": true, "After": true}
)

// transformJournal returns the journal in raw with every path field, and every
// file's recorded bytes (Before and After, which are base64), passed through f.
// Everything else, including fields this code does not know, is kept.
func transformJournal(t *testing.T, raw []byte, f func(text string) string) []byte {
	t.Helper()
	var tree any
	must(t, json.Unmarshal(raw, &tree))
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for key, value := range v {
				text, isText := value.(string)
				switch {
				case isText && journalPathKeys[key]:
					v[key] = f(text)
				case isText && journalFileKeys[key]:
					data, err := base64.StdEncoding.DecodeString(text)
					must(t, err)
					v[key] = base64.StdEncoding.EncodeToString([]byte(f(string(data))))
				default:
					walk(value)
				}
			}
		case []any:
			for _, item := range v {
				walk(item)
			}
		}
	}
	walk(tree)
	out, err := json.MarshalIndent(tree, "", "  ")
	must(t, err)
	return append(out, '\n')
}

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

// canonical is path with its symbolic links resolved (/var is /private/var
// on a Mac), as the code records some paths.
func canonical(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	must(t, err)
	return resolved
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
			r.earlierCollector(earlierLabel("/old/spelling/a"), true)
			r.earlierCollector(earlierLabel("/old/spelling/b"), false)
			r.exe = r.useNewExecutable()
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
			raw, err := os.ReadFile(r.journalPath())
			must(t, err)
			pairs := []string{}
			for _, folder := range [][2]string{{r.userHome, fixtureUserHome}, {r.account, fixtureAccount}, {r.project, fixtureProject}} {
				pairs = append(pairs, canonical(t, folder[0]), folder[1], folder[0], folder[1])
			}
			pairs = append(pairs, firstExe, fixtureBin)
			if r.exe != firstExe {
				pairs = append(pairs, r.exe, fixtureNewBin)
			}
			out := rewritePaths(t, raw, pairs...)
			transformJournal(t, out, func(text string) string {
				for _, temporary := range []string{os.TempDir(), canonical(t, os.TempDir())} {
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
