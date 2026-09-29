package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/terminal"
)

// withPager writes through write. When stdout is a terminal and paging is
// not disabled, the written bytes go to $AGENT_ARCHIVE_PAGER, else $PAGER,
// else less (see defaultPagerCommand), so a long display can be scrolled
// and quit with q. Piped or redirected stdout is never paged; a spawn
// failure falls back to writing stdout directly after a stderr warning.
// Cancelling ctx stops the pager.
func withPager(ctx context.Context, stdout, stderr io.Writer, env pagerDependencies, noPager bool, write func(io.Writer) error) error {
	var buf bytes.Buffer
	if err := write(&buf); err != nil {
		return err
	}
	_, _, err := pageText(ctx, stdout, stderr, env, noPager, false, buf.Bytes())
	return err
}

// lessMouseVersion is the first less with --mouse and --wheel-lines, with
// which the wheel scrolls the text while -X keeps it on the normal screen
// after less quits.
const lessMouseVersion = 551

// lessFitVersion is the first less whose -F prints a text that fits on one
// screen without switching to the alternate screen and back; before it, -F
// without -X showed a short text and wiped it at once.
const lessFitVersion = 530

// defaultPagerCommand is the pager when neither AGENT_ARCHIVE_PAGER nor
// PAGER is set: less, with -F to quit at once when the text fits on one
// screen, -R to show color, and a prompt naming the keys (lessPrompt).
//
// The mouse wheel should scroll the text, not the terminal's history:
//   - less 551 and later get --mouse (the wheel scrolls three lines) and -X
//     (the text stays on the screen after q).
//   - An older less, or one whose version is unknown, runs without -X, on
//     the alternate screen, where terminals turn the wheel into arrow keys.
//   - A less known to be older than 530 keeps -X, since without it -F wipes
//     a short text as soon as it is shown.
//
// stayOpen is for the session browser, which redraws the screen when the
// pager exits: no -F, and -+F overrides an F from $LESS, so less waits for
// q even for a short text; the prompt then says q goes back.
func defaultPagerCommand(env pagerDependencies, stayOpen bool, text []byte) string {
	version, known := env.lessVersion()
	mouse := known && version >= lessMouseVersion
	flags := "-"
	if !stayOpen {
		flags += "F"
	}
	flags += "R"
	if mouse || (known && version < lessFitVersion) {
		flags += "X"
	}
	command := "less " + flags
	if mouse {
		command += " --mouse --wheel-lines=3"
	}
	command += " " + shellQuote("-Ps"+lessPrompt(countLines(text), stayOpen))
	if stayOpen {
		command += " -+F"
	}
	return command
}

// lessPrompt is less's prompt for a text of lines lines, in less's prompt
// language (less(1), PROMPTS): "lines 1-48 of 1210 - " while less knows
// which lines are on the screen, with the percentage once less knows it
// (after reading piped text to its end) and "(END)" at the end, then the
// keys: "arrows/space scroll, / search, q quit". Without line numbers
// (less -n) only the keys remain. It is ASCII with no quote or dollar sign,
// so it passes through shellQuote, sh, and less's option parsing unchanged.
func lessPrompt(lines int, stayOpen bool) string {
	quit := "q quit"
	if stayOpen {
		quit = "q back"
	}
	return fmt.Sprintf(`?ltlines %%lt-%%lb of %d?Pb (%%Pb\%%).?e (END). - .arrows/space scroll, / search, %s`, lines, quit)
}

// countLines is the number of lines less shows for text, before wrapping.
func countLines(text []byte) int {
	n := bytes.Count(text, []byte("\n"))
	if len(text) > 0 && text[len(text)-1] != '\n' {
		n++
	}
	return n
}

// pageText writes text through the pager withPager would choose, and
// reports whether a pager showed it. Cancelling ctx stops the pager.
//
// stayOpen is for the session browser, which redraws the screen when the
// pager exits. The default pager then waits for q even when the text fits
// on one screen (see defaultPagerCommand), and so does a less the user
// chose (-+F overrides an -F from the command or $LESS); waited reports
// that the pager is known to wait for the user. Another pager may have
// returned at once, so the browser waits itself.
func pageText(ctx context.Context, stdout, stderr io.Writer, env pagerDependencies, noPager, stayOpen bool, text []byte) (paged, waited bool, err error) {
	command, chosen, page := resolvePagerCommand(env, noPager, stdout)
	if !page {
		_, err := stdout.Write(text)
		return false, false, err
	}
	switch {
	case !chosen:
		command = defaultPagerCommand(env, stayOpen, text)
		waited = stayOpen
	case stayOpen && isLess(command):
		command += " -+F"
		waited = true
	}
	// A pager stopped because ctx was cancelled (a signal) did run; that is
	// not a failure to fall back from.
	if err := env.runPager(ctx, command, bytes.NewReader(text), stdout, stderr); err != nil && ctx.Err() == nil {
		terminal.Printf(stderr, "agent-archive: warning: pager %q failed (%v); printing directly\n", command, err)
		_, copyErr := stdout.Write(text)
		return false, false, copyErr
	}
	return true, waited, nil
}

// isLess reports whether command runs less by itself, so options can be
// appended to it: no shell syntax (pipes, redirection, substitution,
// quoting, comments, line breaks) and no "--" ending its options.
func isLess(command string) bool {
	if strings.ContainsAny(command, "|;&<>`$()#\\\n'\"") {
		return false
	}
	fields := strings.Fields(command)
	return len(fields) > 0 && filepath.Base(fields[0]) == "less" && !slices.Contains(fields, "--")
}

// resolvePagerCommand returns the pager command the user set, if any. page
// is false when nothing is to be paged: --no-pager, a non-terminal stdout,
// or an empty AGENT_ARCHIVE_PAGER or PAGER, or the value "cat". chosen is
// false when the user set no pager, so the default (defaultPagerCommand)
// runs.
func resolvePagerCommand(env pagerDependencies, noPager bool, stdout io.Writer) (command string, chosen, page bool) {
	if noPager || !env.isTerminal(stdout) {
		return "", false, false
	}
	for _, name := range []string{"AGENT_ARCHIVE_PAGER", "PAGER"} {
		if value, set := env.lookupEnv(name); set {
			if value == "" || value == "cat" {
				return "", false, false
			}
			return value, true, true
		}
	}
	return "", false, true
}

// lessVersion is the version of the less on PATH, as `less --version`
// reports it ("less 668 (...)"); known is false when less cannot be run or
// says something else.
func (e Env) lessVersion() (version int, known bool) {
	if e.LessVersion != nil {
		return e.LessVersion()
	}
	return detectLessVersion()
}

// detectLessVersion runs `less --version` once per process, with a short
// timeout: Env.LessVersion's default. The package's tests replace it with
// one that stops the test, so no test runs the real less.
var detectLessVersion = sync.OnceValues(func() (int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), lessVersionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "less", "--version").Output()
	if err != nil {
		return 0, false
	}
	return parseLessVersion(string(out))
})

// lessVersionTimeout bounds `less --version`, which a broken less on PATH
// could otherwise hang on.
const lessVersionTimeout = 2 * time.Second

// parseLessVersion reads the version number from `less --version` output:
// "less 668 (POSIX regular expressions)", or "less 551" alone.
func parseLessVersion(out string) (int, bool) {
	first, _, _ := strings.Cut(out, "\n")
	fields := strings.Fields(first)
	if len(fields) < 2 || fields[0] != "less" {
		return 0, false
	}
	number, _, _ := strings.Cut(fields[1], ".")
	version, err := strconv.Atoi(number)
	if err != nil || version <= 0 {
		return 0, false
	}
	return version, true
}

// pagerStopDelay is how long a pager asked to stop may take before it is
// killed.
const pagerStopDelay = 3 * time.Second

// runPager runs command through sh. Cancelling ctx sends the pager SIGTERM
// (sh execs a lone command, so the signal reaches the pager itself) and
// waits for it to exit, killing it after pagerStopDelay, so no pager is
// left behind on the terminal.
func (e Env) runPager(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) error {
	if e.RunPager != nil {
		return e.RunPager(ctx, command, stdin, stdout, stderr)
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = pagerStopDelay
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}
