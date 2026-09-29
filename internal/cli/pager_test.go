package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// publishNSessions returns an Env whose store holds n Codex sessions,
// newest capture first when listed, built by cloning the published fixture's
// sidecar with distinct IDs and capture times.
func publishNSessions(t *testing.T, n int) (Env, []string) {
	t.Helper()
	env, mem, id := publishedFixture(t)
	key, err := archive.MetadataObjectKey("codex", id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := mem.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var base archive.Metadata
	if err := json.Unmarshal(raw, &base); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, n)
	ids[0] = id
	for i := 1; i < n; i++ {
		m := base
		m.SessionID = fmt.Sprintf("%s-%d", id, i)
		m.CapturedAt = base.CapturedAt.Add(time.Duration(i) * time.Hour)
		encoded, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		key, err := archive.MetadataObjectKey("codex", m.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if err := mem.Put(context.Background(), key, encoded); err != nil {
			t.Fatal(err)
		}
		ids[i] = m.SessionID
	}
	// Newest first: last inserted has the latest CapturedAt.
	newestFirst := make([]string, n)
	for i := range ids {
		newestFirst[i] = ids[n-1-i]
	}
	return env, newestFirst
}

func TestListLimitCapsNewestSessions(t *testing.T) {
	t.Parallel()
	env, newestFirst := publishNSessions(t, 5)
	var rebuilt, rebuildErr bytes.Buffer
	if code := Run([]string{"list", "--rebuild-index", "--json", "--limit", "0"}, nil, &rebuilt, &rebuildErr, env); code != 0 {
		t.Fatalf("rebuild code=%d stderr=%s", code, rebuildErr.String())
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"list", "--limit", "2", "--verbose"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	listed := listedSessionIDs(out.String())
	if len(listed) != 2 || listed[0] != newestFirst[0] || listed[1] != newestFirst[1] {
		t.Fatalf("listed=%v want newest two %v\n%s", listed, newestFirst[:2], out.String())
	}
	if !strings.Contains(out.String(), "Showing 2 or more session(s).") || !strings.Contains(out.String(), "--limit 0") {
		t.Fatalf("missing truncated footer:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := Run([]string{"list", "--limit", "0", "--verbose"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	listed = listedSessionIDs(out.String())
	if len(listed) != 5 {
		t.Fatalf("limit 0 listed %d, want 5:\n%s", len(listed), out.String())
	}
	if !strings.Contains(out.String(), "5 session(s).") || strings.Contains(out.String(), "Showing") {
		t.Fatalf("unexpected footer for full list:\n%s", out.String())
	}
}

func TestListJSONReportsLimitFields(t *testing.T) {
	t.Parallel()
	env, newestFirst := publishNSessions(t, 4)
	var rebuilt, rebuildErr bytes.Buffer
	if code := Run([]string{"list", "--rebuild-index", "--json", "--limit", "0"}, nil, &rebuilt, &rebuildErr, env); code != 0 {
		t.Fatalf("rebuild code=%d stderr=%s", code, rebuildErr.String())
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"list", "--json", "--limit", "2"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	var doc listDocument
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if doc.Version != listSchemaVersion || doc.Limit != 2 || doc.Returned != 2 || doc.TotalMatched != nil || doc.TotalMatchedKnown || !doc.Truncated {
		t.Fatalf("doc=%+v", doc)
	}
	if len(doc.Sessions) != 2 || doc.Sessions[0].SessionID != newestFirst[0] {
		t.Fatalf("sessions=%v want newest %s", doc.Sessions, newestFirst[0])
	}

	out.Reset()
	if code := Run([]string{"list", "--json", "--limit", "0"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	doc = listDocument{}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Limit != 0 || doc.Returned != 4 || doc.TotalMatched == nil || *doc.TotalMatched != 4 || !doc.TotalMatchedKnown || doc.Truncated || len(doc.Sessions) != 4 {
		t.Fatalf("unlimited doc=%+v", doc)
	}
}

func TestListPagesOnTerminal(t *testing.T) {
	t.Parallel()
	env, _, _ := publishedFixture(t)
	var paged bytes.Buffer
	var sawCommand string
	env.IsTerminal = func(stream any) bool {
		_, ok := stream.(*bytes.Buffer)
		return ok
	}
	env.RunPager = func(_ context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) error {
		sawCommand = command
		_, err := io.Copy(&paged, stdin)
		return err
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"list"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if want := defaultPagerCommand(env, false, paged.Bytes()); sawCommand != want {
		t.Fatalf("pager command=%q", sawCommand)
	}
	if !strings.Contains(paged.String(), "session(s).") || out.Len() != 0 {
		t.Fatalf("paged=%q stdout=%q", paged.String(), out.String())
	}

	// --json never pages, even on a TTY.
	sawCommand = ""
	out.Reset()
	paged.Reset()
	if code := Run([]string{"list", "--json"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if sawCommand != "" || !strings.Contains(out.String(), `"schema_version": 4`) {
		t.Fatalf("json was paged (cmd=%q) or missing:\n%s", sawCommand, out.String())
	}

	// --no-pager and PAGER=cat skip the pager.
	sawCommand = ""
	out.Reset()
	if code := Run([]string{"list", "--no-pager"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("--no-pager: code=%d stderr=%s", code, errOut.String())
	}
	if sawCommand != "" || !strings.Contains(out.String(), "session(s).") {
		t.Fatalf("--no-pager: cmd=%q out=%q", sawCommand, out.String())
	}
	sawCommand = ""
	out.Reset()
	env.LookupEnv = func(key string) (string, bool) {
		if key == "PAGER" {
			return "cat", true
		}
		return "", false
	}
	if code := Run([]string{"list"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("PAGER=cat: code=%d stderr=%s", code, errOut.String())
	}
	if sawCommand != "" || !strings.Contains(out.String(), "session(s).") {
		t.Fatalf("PAGER=cat: cmd=%q out=%q", sawCommand, out.String())
	}
}

func TestListPagerFailureFallsBack(t *testing.T) {
	t.Parallel()
	env, _, _ := publishedFixture(t)
	env.IsTerminal = func(stream any) bool {
		_, ok := stream.(*bytes.Buffer)
		return ok
	}
	env.RunPager = func(context.Context, string, io.Reader, io.Writer, io.Writer) error {
		return errors.New("no less")
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"list"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "pager") || !strings.Contains(out.String(), "session(s).") {
		t.Fatalf("fallback missing: out=%q err=%q", out.String(), errOut.String())
	}
}

func TestResolvePagerCommand(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := Env{
		IsTerminal: func(any) bool { return true },
		LookupEnv:  func(string) (string, bool) { return "", false },
	}
	if cmd, chosen, ok := resolvePagerCommand(env, false, &stdout); !ok || chosen || cmd != "" {
		t.Fatalf("default: %q chosen=%v page=%v", cmd, chosen, ok)
	}
	if _, _, ok := resolvePagerCommand(env, true, &stdout); ok {
		t.Fatal("--no-pager should disable")
	}
	env.IsTerminal = func(any) bool { return false }
	if _, _, ok := resolvePagerCommand(env, false, &stdout); ok {
		t.Fatal("non-TTY should disable")
	}
	env.IsTerminal = func(any) bool { return true }
	env.LookupEnv = func(key string) (string, bool) {
		if key == "AGENT_ARCHIVE_PAGER" {
			return "more", true
		}
		return "", false
	}
	if cmd, chosen, ok := resolvePagerCommand(env, false, &stdout); !ok || !chosen || cmd != "more" {
		t.Fatalf("AGENT_ARCHIVE_PAGER: %q %v %v", cmd, chosen, ok)
	}
	env.LookupEnv = func(key string) (string, bool) {
		if key == "AGENT_ARCHIVE_PAGER" {
			return "", true
		}
		return "less", true
	}
	if _, _, ok := resolvePagerCommand(env, false, &stdout); ok {
		t.Fatal("empty AGENT_ARCHIVE_PAGER should disable even when PAGER is set")
	}
}

// The default less scrolls on the mouse wheel: with --mouse where less has
// it (551 and later), else on the alternate screen (no -X), except for a
// less so old that -F without -X wipes a short text. The browser's less
// waits for q (no -F, and -+F against $LESS) and says q goes back.
func TestDefaultPagerCommandFollowsLessVersion(t *testing.T) {
	t.Parallel()
	text := []byte("one\ntwo\nthree\n")
	quit := shellQuote("-Ps" + lessPrompt(3, false))
	back := shellQuote("-Ps" + lessPrompt(3, true))
	for _, tc := range []struct {
		version  int
		known    bool
		stayOpen bool
		want     string
	}{
		{668, true, false, "less -FRX --mouse --wheel-lines=3 " + quit},
		{551, true, false, "less -FRX --mouse --wheel-lines=3 " + quit},
		{550, true, false, "less -FR " + quit},
		{530, true, false, "less -FR " + quit},
		{529, true, false, "less -FRX " + quit},
		{0, false, false, "less -FR " + quit},
		{668, true, true, "less -RX --mouse --wheel-lines=3 " + back + " -+F"},
		{550, true, true, "less -R " + back + " -+F"},
		{487, true, true, "less -RX " + back + " -+F"},
		{0, false, true, "less -R " + back + " -+F"},
	} {
		env := Env{LessVersion: func() (int, bool) { return tc.version, tc.known }}
		if got := defaultPagerCommand(env, tc.stayOpen, text); got != tc.want {
			t.Errorf("less %d (known %v, stayOpen %v): %q, want %q", tc.version, tc.known, tc.stayOpen, got, tc.want)
		}
	}
}

// The prompt names the keys, says whether q quits or goes back, and counts
// the text's lines itself, since less does not know how many lines piped
// text has until it reaches the end.
func TestLessPromptNamesTheKeys(t *testing.T) {
	t.Parallel()
	if got, want := lessPrompt(1210, false), `?ltlines %lt-%lb of 1210?Pb (%Pb\%).?e (END). - .arrows/space scroll, / search, q quit`; got != want {
		t.Fatalf("prompt %q, want %q", got, want)
	}
	if got := lessPrompt(3, true); !strings.HasSuffix(got, ", q back") {
		t.Fatalf("browser prompt %q", got)
	}
	for text, want := range map[string]int{"": 0, "a": 1, "a\n": 1, "a\nb": 2, "a\nb\n\n": 3} {
		if got := countLines([]byte(text)); got != want {
			t.Errorf("countLines(%q) = %d, want %d", text, got, want)
		}
	}
	for _, prompt := range []string{lessPrompt(12, false), lessPrompt(12, true)} {
		for _, r := range prompt {
			if r > 126 || r < 32 || r == '\'' || r == '"' || r == '$' {
				t.Errorf("prompt %q has %q, which is not plain ASCII or needs quoting", prompt, r)
			}
		}
	}
}

// The default command reaches less exactly as built: sh hands each
// argument, the quoted prompt included, to the program byte for byte. The
// test runs sh with printf in place of less.
func TestDefaultPagerArgumentsSurviveTheShell(t *testing.T) {
	t.Parallel()
	for _, stayOpen := range []bool{false, true} {
		for _, version := range []int{668, 540} {
			env := Env{LessVersion: func() (int, bool) { return version, true }}
			command := defaultPagerCommand(env, stayOpen, []byte("x\n"))
			args, ok := strings.CutPrefix(command, "less ")
			if !ok {
				t.Fatalf("command %q does not run less", command)
			}
			var out, errOut bytes.Buffer
			cmd := exec.CommandContext(t.Context(), "sh", "-c", `printf '%s\n' `+args)
			cmd.Stdout, cmd.Stderr = &out, &errOut
			if err := cmd.Run(); err != nil {
				t.Fatalf("sh -c %q: %v %s", args, err, errOut.String())
			}
			got := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
			if want := "-Ps" + lessPrompt(1, stayOpen); !slices.Contains(got, want) {
				t.Fatalf("less would get %q, want the argument %q", got, want)
			}
			if stayOpen && got[len(got)-1] != "-+F" {
				t.Fatalf("browser pager arguments %q do not end with -+F", got)
			}
		}
	}
}

// A pager the user chose runs as given: no prompt or mouse options.
func TestUserPagerRunsAsGiven(t *testing.T) {
	t.Parallel()
	for _, pager := range []string{"less -R", "most", "less"} {
		var got string
		env := Env{
			IsTerminal:  func(any) bool { return true },
			LookupEnv:   func(key string) (string, bool) { return pager, key == "PAGER" },
			LessVersion: func() (int, bool) { return 668, true },
			RunPager: func(_ context.Context, command string, _ io.Reader, _, _ io.Writer) error {
				got = command
				return nil
			},
		}
		var out bytes.Buffer
		if _, _, err := pageText(context.Background(), &out, io.Discard, env, false, false, []byte("x\n")); err != nil || got != pager {
			t.Errorf("PAGER=%q ran %q (%v)", pager, got, err)
		}
	}
}

func TestParseLessVersion(t *testing.T) {
	t.Parallel()
	for out, want := range map[string]int{
		"less 668 (POSIX regular expressions)\nCopyright (C) 1984-2024  Mark Nudelman\n": 668,
		"less 551\n":                             551,
		"less 643 (PCRE2 regular expressions)\n": 643,
		"less 1.2\n":                             1,
		"BusyBox v1.36.1\n":                      0,
		"":                                       0,
		"less version unknown\n":                 0,
	} {
		version, known := parseLessVersion(out)
		if version != want || known != (want > 0) {
			t.Errorf("parseLessVersion(%q) = %d, %v; want %d", out, version, known, want)
		}
	}
}

// pagedRun runs args with stdout a terminal (color or not) whose pager
// records what it is given, and returns the pager's text and command (empty
// when nothing was paged) and what reached stdout directly.
func pagedRun(t *testing.T, env Env, color bool, args ...string) (paged, command, direct string) {
	t.Helper()
	stdout := &screenOutput{color: color}
	env.IsTerminal = func(stream any) bool { return stream == any(stdout) }
	var text bytes.Buffer
	env.RunPager = func(_ context.Context, cmd string, in io.Reader, _, _ io.Writer) error {
		command = cmd
		_, err := io.Copy(&text, in)
		return err
	}
	var errOut bytes.Buffer
	if code := Run(args, nil, stdout, &errOut, env); code != 0 {
		t.Fatalf("%v: code=%d stderr=%s", args, code, errOut.String())
	}
	return text.String(), command, stdout.String()
}

// planFileName is the random name of a saved purge plan, which differs
// between runs.
var planFileName = regexp.MustCompile(`[0-9a-f]{32}\.json`)

// show's summary, status, and purge plan are paged on a terminal: the
// pager gets exactly what a pipe gets, in color when the terminal has it,
// and --no-pager prints it directly.
func TestLongDisplaysArePagedOnATerminal(t *testing.T) {
	t.Parallel()
	showEnv, _, id := publishedFixture(t)
	statusEnv := testEnv(t, t.TempDir(), time.Now())
	purgeHome := t.TempDir()
	setUpTestConfig(t, purgeHome, t.TempDir(), time.Now())
	purgeEnv := testEnv(t, purgeHome, time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC))
	for _, tc := range []struct {
		name  string
		env   Env
		args  []string
		color bool
	}{
		{"show", showEnv, []string{"show", id}, true},
		{"status", statusEnv, []string{"status"}, true},
		{"purge plan", purgeEnv, []string{"purge", "plan"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var piped, pipedErr bytes.Buffer
			if code := Run(tc.args, nil, &piped, &pipedErr, tc.env); code != 0 {
				t.Fatalf("piped: code=%d stderr=%s", code, pipedErr.String())
			}
			want := planFileName.ReplaceAllString(piped.String(), "PLAN.json")
			if strings.Contains(want, "\x1b") {
				t.Fatalf("piped output has escapes:\n%q", want)
			}
			paged, command, direct := pagedRun(t, tc.env, false, tc.args...)
			if !strings.HasPrefix(command, "less -FRX --mouse ") || direct != "" || planFileName.ReplaceAllString(paged, "PLAN.json") != want {
				t.Fatalf("pager %q got\n%q\nstdout %q\nwant\n%q", command, paged, direct, want)
			}
			paged, command, direct = pagedRun(t, tc.env, false, append(slices.Clone(tc.args), "--no-pager")...)
			if command != "" || paged != "" || planFileName.ReplaceAllString(direct, "PLAN.json") != want {
				t.Fatalf("--no-pager: pager %q, stdout\n%q\nwant\n%q", command, direct, want)
			}
			if tc.color {
				paged, _, _ = pagedRun(t, tc.env, true, tc.args...)
				if !strings.Contains(paged, "\x1b[") {
					t.Fatalf("paged without color on a color terminal:\n%q", paged)
				}
			}
		})
	}
}

// --json is never paged.
func TestJSONDisplaysAreNotPaged(t *testing.T) {
	t.Parallel()
	showEnv, _, id := publishedFixture(t)
	statusEnv := testEnv(t, t.TempDir(), time.Now())
	for _, tc := range []struct {
		env  Env
		args []string
	}{
		{showEnv, []string{"show", id, "--json"}},
		{statusEnv, []string{"status", "--json"}},
	} {
		if _, command, direct := pagedRun(t, tc.env, true, tc.args...); command != "" || !strings.HasPrefix(direct, "{") {
			t.Errorf("%v: pager %q, stdout %q", tc.args, command, direct)
		}
	}
}
