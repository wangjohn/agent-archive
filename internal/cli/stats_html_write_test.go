package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// listNames is the names in dir, so a test can see that a run left nothing
// but the page behind.
func listNames(tb testing.TB, dir string) []string {
	tb.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		tb.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// The page is moved into place whole, so a run leaves the folder with the page
// and nothing else: no temporary file, at mode 0600 whatever the file was
// before.
func TestStatsHTMLFileIsWrittenWholeAndLeavesNoTemporaryFile(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.html")
	if _, errOut, code := runStats(t, env, 0, "--html", "--output", path); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if names := listNames(t, dir); len(names) != 1 || names[0] != "stats.html" {
		t.Fatalf("the folder holds %v, want only stats.html", names)
	}

	// A file that was world-readable comes out 0600, replaced in one step.
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := runStats(t, env, 0, "--html", "--output", path, "--force"); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode after --force = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	if names := listNames(t, dir); len(names) != 1 {
		t.Fatalf("the folder holds %v after --force", names)
	}
}

// A page that cannot be written leaves the existing file as it was, and no
// temporary file.
func TestStatsHTMLFailedWriteKeepsTheExistingFile(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root can write to a read-only folder")
	}
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.html")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	out, errOut, code := runStats(t, env, 0, "--html", "--output", path, "--force")
	if code != 1 || out != "" || !strings.Contains(errOut, "agent-archive: stats:") {
		t.Fatalf("code=%d stdout=%q stderr=%q, want exit 1", code, out, errOut)
	}
	if kept, _ := os.ReadFile(path); string(kept) != "keep me" {
		t.Fatalf("the existing file was changed: %q", kept)
	}
	if names := listNames(t, dir); len(names) != 1 {
		t.Fatalf("a failed run left %v behind", names)
	}
}

// A symbolic link is never written through or replaced, and neither is a
// device or a pipe: --output /dev/null --force must not touch /dev.
func TestStatsHTMLRefusesLinksAndSpecialFiles(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	dir := t.TempDir()
	target := filepath.Join(dir, "target.html")
	if err := os.WriteFile(target, []byte("the target"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.html")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling.html")
	if err := os.Symlink(filepath.Join(dir, "nowhere.html"), dangling); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		want string
	}{
		{link, "is a symbolic link"}, {dangling, "is a symbolic link"}, {os.DevNull, "is not an ordinary file"},
	} {
		for _, force := range []bool{false, true} {
			args := []string{"--html", "--output", tc.path}
			if force {
				args = append(args, "--force")
			}
			out, errOut, code := runStats(t, env, 0, args...)
			if code != 2 || out != "" || !strings.Contains(errOut, tc.want) {
				t.Errorf("%s (force=%v): code=%d stdout=%q stderr=%q, want exit 2 mentioning %q", tc.path, force, code, out, errOut, tc.want)
			}
		}
	}
	if kept, _ := os.ReadFile(target); string(kept) != "the target" {
		t.Errorf("the link's target was written: %q", kept)
	}
	if _, err := os.Lstat(filepath.Join(dir, "nowhere.html")); err == nil {
		t.Error("a dangling link's target was created")
	}
}

// writeStatsHTMLFile never replaces what appeared at the path after the flags
// were checked, without force.
func TestWriteStatsHTMLFileWithoutForceNeverReplaces(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.html")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := writeStatsHTMLFile(path, []byte("second"), false)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want already exists", err)
	}
	if kept, _ := os.ReadFile(path); string(kept) != "first" {
		t.Fatalf("the file was replaced: %q", kept)
	}
	if names := listNames(t, dir); len(names) != 1 {
		t.Fatalf("the folder holds %v", names)
	}
	if err := writeStatsHTMLFile(path, []byte("second"), true); err != nil {
		t.Fatal(err)
	}
	if replaced, _ := os.ReadFile(path); string(replaced) != "second" {
		t.Fatalf("--force did not replace: %q", replaced)
	}
}

// failingWriter is a standard output that cannot be written: a full disk or a
// closed pipe.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

// A page that could not be written to standard output is an error, not a
// success with half a page.
func TestStatsHTMLReportsAFailedWriteToStdout(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	var errOut bytes.Buffer
	code := Run([]string{"stats", "--html"}, nil, failingWriter{}, &errOut, env)
	if code != 1 || !strings.Contains(errOut.String(), "no space left on device") {
		t.Fatalf("code=%d stderr=%q, want exit 1 naming the error", code, errOut.String())
	}
}
