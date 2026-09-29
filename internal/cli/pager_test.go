package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"syscall"
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
	env.RunPager = func(_ context.Context, command string, _ []string, stdin io.Reader, stdout, stderr io.Writer) error {
		sawCommand = command
		_, err := io.Copy(&paged, stdin)
		return err
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"list"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if want := defaultPagerCommand(env, "less", false, paged.Bytes()); sawCommand != want {
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
	env.RunPager = func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error {
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
	// A bare less, as oh-my-zsh sets PAGER, gets the default treatment.
	for _, pager := range []string{"less", "/usr/bin/less", " less "} {
		env.LookupEnv = func(key string) (string, bool) { return pager, key == "PAGER" }
		if cmd, chosen, ok := resolvePagerCommand(env, false, &stdout); !ok || chosen || cmd != strings.TrimSpace(pager) {
			t.Fatalf("PAGER=%q: %q chosen=%v page=%v", pager, cmd, chosen, ok)
		}
	}
}

// lessPrompts is the -Ps, -Pm, and -PM options defaultPagerCommand passes
// for a text of lines lines, quoted for sh.
func lessPrompts(lines int, stayOpen bool) string {
	prompt := lessPrompt(lines, stayOpen)
	return shellQuote("-Ps"+prompt) + " " + shellQuote("-Pm"+prompt) + " " + shellQuote("-PM"+prompt)
}

// The default less scrolls on the mouse wheel: with --mouse where less has
// it (551 and later), else on the alternate screen (no -X), except for a
// less so old that -F without -X wipes a short text. A less of unknown
// version gets no prompt or mouse options. The browser's less waits for q
// (no -F, and -+F against $LESS) and says q goes back.
func TestDefaultPagerCommandFollowsLessVersion(t *testing.T) {
	t.Parallel()
	text := []byte("one\ntwo\nthree\n")
	quit, back := lessPrompts(3, false), lessPrompts(3, true)
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
		{0, false, false, "less -FR"},
		{668, true, true, "less -RX --mouse --wheel-lines=3 " + back + " -+F"},
		{550, true, true, "less -R " + back + " -+F"},
		{487, true, true, "less -RX " + back + " -+F"},
		{0, false, true, "less -R -+F"},
	} {
		env := Env{LessVersion: func(string) (int, bool) { return tc.version, tc.known }}
		if got := defaultPagerCommand(env, "less", tc.stayOpen, text); got != tc.want {
			t.Errorf("less %d (known %v, stayOpen %v): %q, want %q", tc.version, tc.known, tc.stayOpen, got, tc.want)
		}
	}
}

// The prompt names the keys, says whether q quits or goes back, and counts
// the text's lines itself, since less does not know how many lines piped
// text has until it reaches the end. As less's own prompts do, it shows
// (END) at the end in place of the percentage, which less computes from the
// top line and so never reaches 100%.
func TestLessPromptNamesTheKeys(t *testing.T) {
	t.Parallel()
	if got, want := lessPrompt(1210, false), `?ltlines %lt-%lb of 1210 ?e(END) :?Pb(%Pb\%) ..- .arrows/space scroll, / search, q quit`; got != want {
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
// argument, the quoted prompts included, to the program byte for byte. The
// test runs sh with printf in place of less. A bare less the user set runs
// by the path the user gave.
func TestDefaultPagerArgumentsSurviveTheShell(t *testing.T) {
	t.Parallel()
	for _, stayOpen := range []bool{false, true} {
		for _, version := range []int{668, 540} {
			env := Env{LessVersion: func(string) (int, bool) { return version, true }}
			command := defaultPagerCommand(env, "/usr/bin/less", stayOpen, []byte("x\n"))
			args, ok := strings.CutPrefix(command, "/usr/bin/less ")
			if !ok {
				t.Fatalf("command %q does not run /usr/bin/less", command)
			}
			var out, errOut bytes.Buffer
			cmd := exec.CommandContext(t.Context(), "sh", "-c", `printf '%s\n' `+args)
			cmd.Stdout, cmd.Stderr = &out, &errOut
			if err := cmd.Run(); err != nil {
				t.Fatalf("sh -c %q: %v %s", args, err, errOut.String())
			}
			got := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
			for _, option := range []string{"-Ps", "-Pm", "-PM"} {
				if want := option + lessPrompt(1, stayOpen); !slices.Contains(got, want) {
					t.Fatalf("less would get %q, want the argument %q", got, want)
				}
			}
			if stayOpen && got[len(got)-1] != "-+F" {
				t.Fatalf("browser pager arguments %q do not end with -+F", got)
			}
		}
	}
}

// A pager the user chose runs as given, with LESS=FRX and LV=-c added when
// they are unset, as git does, so a less the user named without options
// still leaves a short text on the screen. A bare less instead gets the
// default command, by the path the user gave, and nothing added.
func TestUserPagerRunsAsGiven(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		pager   string
		less    string
		lv      string
		want    string
		wantEnv []string
	}{
		{pager: "less -R", want: "less -R", wantEnv: []string{"LESS=FRX", "LV=-c"}},
		{pager: "most", less: "-iMR", want: "most", wantEnv: []string{"LV=-c"}},
		{pager: "lv", less: "-R", lv: "-a", want: "lv"},
		{pager: "less", want: "less -FRX --mouse --wheel-lines=3 " + lessPrompts(1, false)},
		{pager: "/usr/bin/less", less: "-R", want: "/usr/bin/less -FRX --mouse --wheel-lines=3 " + lessPrompts(1, false)},
	} {
		var got string
		var gotEnv []string
		var versionOf string
		env := Env{
			IsTerminal: func(any) bool { return true },
			LookupEnv: func(key string) (string, bool) {
				values := map[string]string{"PAGER": tc.pager, "LESS": tc.less, "LV": tc.lv}
				value := values[key]
				return value, value != ""
			},
			LessVersion: func(program string) (int, bool) { versionOf = program; return 668, true },
			RunPager: func(_ context.Context, command string, environment []string, _ io.Reader, _, _ io.Writer) error {
				got, gotEnv = command, environment
				return nil
			},
		}
		var out bytes.Buffer
		if _, _, err := pageText(context.Background(), &out, io.Discard, env, false, false, []byte("x\n")); err != nil || got != tc.want || !slices.Equal(gotEnv, tc.wantEnv) {
			t.Errorf("PAGER=%q LESS=%q LV=%q ran %q with %q (%v), want %q with %q", tc.pager, tc.less, tc.lv, got, gotEnv, err, tc.want, tc.wantEnv)
		}
		if strings.Contains(tc.pager, "less") && !strings.Contains(tc.pager, " ") && versionOf != tc.pager {
			t.Errorf("PAGER=%q: version asked of %q", tc.pager, versionOf)
		}
	}
}

// The environment reaches the pager's process: the default runPager adds
// it to the process's own. The test runs sh with a command that prints the
// variable in place of a pager.
func TestPagerEnvironmentReachesThePager(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := (Env{}).runPager(t.Context(), `printf '%s' "$AGENT_ARCHIVE_TEST_PAGER_VAR"`, []string{"AGENT_ARCHIVE_TEST_PAGER_VAR=FRX"}, strings.NewReader(""), &out, io.Discard); err != nil || out.String() != "FRX" {
		t.Fatalf("pager saw %q (%v)", out.String(), err)
	}
}

// While a paged display's pager runs, Ctrl-C is left to the pager: the
// command neither exits nor stops it. SIGTERM stops the pager, then the
// command exits as the signal would have.
func TestPagerKeepsCtrlCAndStopsOnSIGTERM(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		signals  []os.Signal
		wantExit int
	}{
		{[]os.Signal{os.Interrupt}, -1},
		{[]os.Signal{os.Interrupt, syscall.SIGTERM}, 128 + int(syscall.SIGTERM)},
		{[]os.Signal{syscall.SIGHUP}, 128 + int(syscall.SIGHUP)},
	} {
		env := testEnv(t, t.TempDir(), time.Now())
		var stdout bytes.Buffer
		env.IsTerminal = func(stream any) bool { return stream == any(&stdout) }
		signals := make(chan os.Signal)
		stopped := false
		env.Interrupts = func() (<-chan os.Signal, func()) { return signals, func() { stopped = true } }
		exited := -1
		env.exitProcess = func(code int) { exited = code }
		env.RunPager = func(ctx context.Context, _ string, _ []string, _ io.Reader, _, _ io.Writer) error {
			for _, sig := range tc.signals {
				signals <- sig
			}
			if tc.wantExit < 0 {
				// Ctrl-C alone: the pager runs on until the user quits it.
				select {
				case <-ctx.Done():
					return errors.New("the pager was stopped by Ctrl-C")
				case <-time.After(50 * time.Millisecond):
					return nil
				}
			}
			<-ctx.Done()
			return ctx.Err()
		}
		err := withPager(context.Background(), &stdout, io.Discard, env, false, func(w io.Writer) error {
			_, err := io.WriteString(w, "text\n")
			return err
		})
		if err != nil || exited != tc.wantExit || !stopped || stdout.Len() != 0 {
			t.Errorf("signals %v: err=%v exit=%d (want %d) handler stopped=%v stdout=%q", tc.signals, err, exited, tc.wantExit, stopped, stdout.String())
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
	env.RunPager = func(_ context.Context, cmd string, _ []string, in io.Reader, _, _ io.Writer) error {
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
