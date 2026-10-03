package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// None of these tests is parallel: newRefreshInstall replaces launchctl.

// setup --refresh rewrites the collector's plist from the program and the
// environment of the plist that is there: the executable becomes the running
// one, AGENT_ARCHIVE_HOME is dropped and written again from the data
// directory in use, every other variable stays as it was (whatever this shell
// has), and the rest of the file is the template's again. The goldens are the
// plists a refresh leaves; the paths in them are tokens (@EXE@, @DATA_HOME@,
// @LABEL@), so they read the same in every temporary directory.
//
// hand-edited documents what refresh does today, not what it should do: a
// person's own keys (KeepAlive, Nice), their StartInterval of 300, and an
// extra program argument (--verbose) are dropped and the template's written
// instead; only the environment survives. A change that keeps them updates
// that golden on purpose.
func TestRefreshRewritesThePlistFromTheOneThatIsThere(t *testing.T) {
	for _, tc := range []struct {
		name           string
		defaultInstall bool
	}{
		{"default-s3", true},
		{"nondefault-stale-data-home", false},
		{"no-data-home", false},
		{"hand-edited", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefreshInstall(t, tc.defaultInstall, true, modeMissing)
			// The shell running the refresh has other AWS files and another
			// PATH than the collector was given; refresh must not take them.
			r.env.LookupEnv = shellEnvironment(map[string]string{"AWS_CONFIG_FILE": "/shell/aws-config", "AWS_PROFILE": "shell", "PATH": "/shell/bin"})
			before, err := os.ReadFile(filepath.Join("testdata", "scheduler-refresh", tc.name+".before.plist"))
			must(t, err)
			r.writePlist(t, r.expand(string(before)))

			code, stdout, stderr := r.run(t)
			if code != 0 || stderr != "" || !strings.Contains(stdout, "the background collector's plist (its job is not loaded, and was left so)") {
				t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
			}
			after, err := os.ReadFile(r.plist)
			must(t, err)
			golden.Check(t, filepath.Join("testdata", "scheduler-refresh", tc.name+".after.plist"), []byte(r.tokenize(string(after))))
			if tc.defaultInstall != (r.label() == defaultCollectorLabel) {
				t.Errorf("label %s", r.label())
			}
			if !strings.Contains(string(after), "<string>"+r.newExe+"</string>") {
				t.Errorf("the plist does not run %s", r.newExe)
			}
		})
	}
}

// A plist that already runs the executable is not rewritten, though the file
// differs from what setup writes in every other way: the check is the program
// alone.
func TestRefreshLeavesAPlistThatRunsTheExecutableAlone(t *testing.T) {
	r := newRefreshInstall(t, false, false, modeLoaded)
	before, err := os.ReadFile(filepath.Join("testdata", "scheduler-refresh", "hand-edited.before.plist"))
	must(t, err)
	edited := r.expand(strings.ReplaceAll(string(before), "@OLD_EXE@", "@EXE@"))
	r.writePlist(t, edited)
	code, stdout, stderr := r.run(t)
	if code != 0 || stdout != "nothing to refresh\n" || stderr != "" {
		t.Fatalf("exit %d\n%q\n%q", code, stdout, stderr)
	}
	if got := readText(t, r.plist); got != edited {
		t.Errorf("the plist changed:\n%s", got)
	}
	if calls := r.launchd.argv(); len(calls) != 0 {
		t.Errorf("launchctl: %v", calls)
	}
}

// An absent plist and an invalid one are different: refresh does not create
// a plist (that is setup's) and goes on with the hooks and skills, but one it
// cannot read is a refusal before anything is written, so the next step is
// setup, which writes it again.
func TestRefreshAbsentAndInvalidPlistsAreDifferent(t *testing.T) {
	r := newRefreshInstall(t, false, true, modeLoaded)
	before := tree(t, r.home, r.userHome)
	for _, tc := range []struct {
		name  string
		plist string
		cause string
	}{
		{"empty file", " ", "LaunchAgent has no ProgramArguments"},
		{"not a plist", "just some words\n", "LaunchAgent has no ProgramArguments"},
		{"no program arguments", "<plist><dict><key>Label</key><string>x</string></dict></plist>", "LaunchAgent has no ProgramArguments"},
		{"empty program arguments", "<plist><dict><key>ProgramArguments</key><array></array></dict></plist>", "LaunchAgent ProgramArguments is empty"},
		{"empty program", "<plist><dict><key>ProgramArguments</key><array><string>  </string></array></dict></plist>", "LaunchAgent program is empty"},
		{"cut short", "<plist><dict><key>ProgramArguments</key><array><string>/old/agent-archive", "read LaunchAgent: XML syntax error on line 1: unexpected EOF"},
	} {
		// Each is refused before anything is written, so one installation serves.
		r.writePlist(t, tc.plist)
		snapshot := tree(t, r.home, r.userHome)
		code, stdout, stderr := r.run(t)
		want := "agent-archive: setup --refresh: " + r.plist + " cannot be read (" + tc.cause + "); run agent-archive setup to write it again. Nothing was changed\n"
		if code != 1 || stdout != "" || stderr != want {
			t.Fatalf("%s: exit %d\n%q\n%q\nwant %q", tc.name, code, stdout, stderr, want)
		}
		if changed := differences(snapshot, tree(t, r.home, r.userHome)); len(changed) != 0 {
			t.Errorf("%s: a refused refresh changed %v", tc.name, changed)
		}
	}
	if calls := r.launchd.argv(); len(calls) != 0 {
		t.Errorf("launchctl: %v", calls)
	}
	// Absent, the same refresh goes ahead without it. (How many skill files
	// there are is the skills' business, pinned with them.)
	must(t, os.Remove(r.plist))
	code, stdout, stderr := r.run(t)
	first, _, _ := strings.Cut(stdout, "\n")
	if code != 0 || stderr != "" || !regexp.MustCompile(`^refreshed Codex and Claude Code hooks and \d+ skill files;`).MatchString(first) || strings.Contains(stdout, "collector") {
		t.Fatalf("absent: exit %d\n%s\n%s", code, stdout, stderr)
	}
	if _, err := os.Stat(r.plist); !os.IsNotExist(err) {
		t.Errorf("refresh wrote a plist: %v", err)
	}
	if calls := r.launchd.argv(); len(calls) != 0 {
		t.Errorf("launchctl: %v", calls)
	}
	if len(differences(before, tree(t, r.home, r.userHome))) == 0 {
		t.Error("the refresh changed nothing")
	}
}

// A plist refresh cannot read because of the file system, not its text, is
// refused with the operating system's words and without the advice to run
// setup, and changes nothing.
func TestRefreshRefusesAPlistItCannotOpen(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	r := newRefreshInstall(t, false, true, modeLoaded)
	before := tree(t, r.home, r.userHome)
	must(t, os.Chmod(r.plist, 0))
	t.Cleanup(func() { _ = os.Chmod(r.plist, 0o600) })
	code, stdout, stderr := r.run(t)
	must(t, os.Chmod(r.plist, 0o600))
	if code != 1 || stdout != "" || !strings.HasPrefix(stderr, "agent-archive: setup --refresh: open "+r.plist+": permission denied") || !strings.HasSuffix(stderr, ". Nothing was changed\n") {
		t.Fatalf("exit %d\n%q\n%q", code, stdout, stderr)
	}
	if strings.Contains(stderr, "run agent-archive setup") {
		t.Errorf("stderr sends the person to setup: %q", stderr)
	}
	if changed := differences(before, tree(t, r.home, r.userHome)); len(changed) != 0 {
		t.Errorf("a refused refresh changed %v", changed)
	}
	if calls := r.launchd.argv(); len(calls) != 0 {
		t.Errorf("launchctl: %v", calls)
	}
}
