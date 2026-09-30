package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `stats --html` on a pipe writes the page and nothing else: no spinner, no
// note, exit 0. The page is the default, shareable one: no project names.
func TestStatsHTMLToStdoutIsJustThePage(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	out, errOut, code := runStats(t, env, 0, "--html")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if !strings.HasPrefix(out, "<!DOCTYPE html>") || !strings.HasSuffix(strings.TrimSpace(out), "</html>") {
		t.Fatalf("stdout is not just a page:\n%.300s", out)
	}
	for _, want := range []string{"Tokens by day", "Cost by model", "project A", "Generated 2026-09-29 12:00 UTC"} {
		if !strings.Contains(out, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	for _, secret := range []string{"proj-api", "dotfiles", "claude-big"} {
		if strings.Contains(out, secret) {
			t.Errorf("the shareable page contains %q", secret)
		}
	}
	if strings.Contains(out, "Reading sessions") {
		t.Error("a spinner line is in the page")
	}
}

func TestStatsHTMLIncludeProjectNames(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	out := mustRunStats(t, env, 0, "--html", "--include-project-names", "--by", "project")
	for _, name := range []string{"proj-api", "dotfiles"} {
		if !strings.Contains(out, name) {
			t.Errorf("the page lacks the project %q", name)
		}
	}
	if strings.Contains(out, "project A") {
		t.Error("real names were asked for, but the page has stand-ins")
	}
	if !strings.Contains(out, "By project") {
		t.Error("--by project adds no breakdown")
	}
}

// --output writes the page to a file with mode 0600 and says so on stderr;
// stdout stays empty. An existing file is never replaced without --force.
func TestStatsHTMLOutputFile(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	path := filepath.Join(t.TempDir(), "stats.html")

	out, errOut, code := runStats(t, env, 0, "--html", "--output", path)
	if code != 0 || out != "" || !strings.Contains(errOut, "stats: wrote "+path) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(data, []byte("<!DOCTYPE html>")) {
		t.Fatalf("the file is not a page: %v\n%.200s", err, data)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}

	// A second run refuses, before reading the archive, and leaves the file.
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code = runStats(t, env, 0, "--html", "--output", path)
	if code != 2 || out != "" || !strings.Contains(errOut, "already exists; pass --force to replace it") {
		t.Fatalf("overwrite: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if kept, _ := os.ReadFile(path); string(kept) != "keep me" {
		t.Fatalf("the existing file was changed: %q", kept)
	}

	// With --force it is replaced.
	if _, errOut, code = runStats(t, env, 0, "--html", "--output", path, "--force"); code != 0 {
		t.Fatalf("--force: code=%d stderr=%q", code, errOut)
	}
	if replaced, _ := os.ReadFile(path); !bytes.HasPrefix(replaced, []byte("<!DOCTYPE html>")) {
		t.Fatalf("--force did not replace the file: %.100s", replaced)
	}
}

// A file that cannot be written is reported before the archive is read.
func TestStatsHTMLOutputProblemsAreUsageErrors(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	dir := t.TempDir()
	for _, tc := range []struct {
		path string
		want string
	}{
		{filepath.Join(dir, "missing", "stats.html"), "does not exist"},
		{dir, "is a directory"},
	} {
		out, errOut, code := runStats(t, env, 0, "--html", "--output", tc.path, "--force")
		if code != 2 || out != "" || !strings.Contains(errOut, tc.want) {
			t.Errorf("%s: code=%d stdout=%q stderr=%q, want exit 2 mentioning %q", tc.path, code, out, errOut, tc.want)
		}
	}
}

func TestStatsHTMLFlagCombinations(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	file := filepath.Join(t.TempDir(), "x.html")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--html", "--json"}, "--html and --json cannot be combined"},
		{[]string{"--output", file}, "--output applies only to --html"},
		{[]string{"--include-project-names"}, "--include-project-names applies only to --html"},
		{[]string{"--html", "--force"}, "--force applies only to --output"},
	} {
		out, errOut, code := runStats(t, env, 0, tc.args...)
		if code != 2 || out != "" || !strings.Contains(errOut, tc.want) {
			t.Errorf("%v: code=%d stdout=%q stderr=%q, want exit 2 mentioning %q", tc.args, code, out, errOut, tc.want)
		}
	}
	if _, err := os.Stat(file); err == nil {
		t.Error("a rejected run created the file")
	}
}

// The page is not dumped onto a terminal: --html on a terminal needs --output.
func TestStatsHTMLWontFillATerminal(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	env.IsTerminal = func(any) bool { return true }
	out, errOut, code := runStats(t, env, 0, "--html")
	if code != 2 || out != "" || !strings.Contains(errOut, "give --output FILE") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	// With --output the terminal only sees the note.
	path := filepath.Join(t.TempDir(), "stats.html")
	if _, errOut, code := runStats(t, env, 0, "--html", "--output", path); code != 0 || !strings.Contains(errOut, "stats: wrote") {
		t.Fatalf("with --output: code=%d stderr=%q", code, errOut)
	}
}

// An archive with no sessions in the window still gets a page, which says why
// and what to try; filters are named and escaped.
func TestStatsHTMLEmptyWindowAndFilters(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	empty := mustRunStats(t, env, 0, "--html")
	for _, want := range []string{"Nothing to show yet", "No archived sessions in the last 30 days", "agent-archive status"} {
		if !strings.Contains(empty, want) {
			t.Errorf("the empty page lacks %q", want)
		}
	}
	publishStatsFixture(t, mem)
	filtered := mustRunStats(t, env, 0, "--html", "--model", `<b>none</b>`, "--hook-captured")
	for _, want := range []string{"No archived sessions match these filters", "model &lt;b&gt;none&lt;/b&gt;", "hook-captured sessions"} {
		if !strings.Contains(filtered, want) {
			t.Errorf("the filtered empty page lacks %q\n%s", want, filtered)
		}
	}
	if strings.Contains(filtered, "<b>none</b>") {
		t.Error("the model filter reached the page unescaped")
	}
}

// --prices is applied to the page and the footer says so.
func TestStatsHTMLNamesTheOverriddenPrices(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	out := mustRunStats(t, env, 0, "--html", "--prices", goldenPrices)
	if !strings.Contains(out, "Prices golden-1, as of 2026-09-29, with your own price file applied.") {
		t.Errorf("the footer does not name the prices:\n%s", out[max(strings.Index(out, `<footer`), 0):])
	}
}
