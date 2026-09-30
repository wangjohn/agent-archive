package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// The interactive screen opens only when stdin and stdout are terminals,
// interaction is on, and no flag asks for a printed page. Every other run
// prints exactly what it printed before, and never asks for the keys.
func TestStatsInteractiveOnlyWhereItShould(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	// The reference: the overview on a terminal-less run, at 100 columns.
	static := mustRunStats(t, env, 100, "--prices", goldenPrices)

	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		args []string
		// stdin and stdout are whether each is a terminal to the Env.
		stdin          bool
		stdout         bool
		nonInteractive string
		term           string
		// same is whether the output is the plain overview.
		same bool
	}{
		{name: "piped stdout", stdin: true, stdout: false, same: true},
		{name: "piped stdin", stdin: false, stdout: true, same: true},
		{name: "neither", same: true},
		{name: "NONINTERACTIVE=1", stdin: true, stdout: true, nonInteractive: "1", same: true},
		{name: "NONINTERACTIVE=true", stdin: true, stdout: true, nonInteractive: "true", same: true},
		{name: "TERM=dumb", stdin: true, stdout: true, term: "dumb"},
		{name: "--no-pager", args: []string{"--no-pager"}, stdin: true, stdout: true, same: true},
		{name: "--view overview", args: []string{"--view", "overview"}, stdin: true, stdout: true, same: true},
		{name: "--view detail", args: []string{"--view", "detail"}, stdin: true, stdout: true},
		{name: "--detail", args: []string{"--detail"}, stdin: true, stdout: true},
		{name: "--by day", args: []string{"--by", "day"}, stdin: true, stdout: true},
		{name: "--by project", args: []string{"--by", "project"}, stdin: true, stdout: true},
		{name: "--json", args: []string{"--json"}, stdin: true, stdout: true},
		{name: "--html --output", args: []string{"--html", "--output", filepath.Join(dir, "page.html")}, stdin: true, stdout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := runStatsOnTerminal(t, fixedTerminal{100, 40}, func(e *Env) {
				stdout := e.IsTerminal
				e.IsTerminal = func(stream any) bool {
					// The stream is stdin when it is the strings.Reader.
					if _, isReader := stream.(*strings.Reader); isReader {
						return tc.stdin
					}
					return tc.stdout && stdout(stream)
				}
				if tc.nonInteractive != "" || tc.term != "" {
					e.LookupEnv = func(name string) (string, bool) {
						switch {
						case name == envNonInteractive && tc.nonInteractive != "":
							return tc.nonInteractive, true
						case name == "TERM" && tc.term != "":
							return tc.term, true
						}
						return "", false
					}
				}
			}, []string{"q"}, tc.args...)
			if run.code != 0 || run.stderr != "" && !strings.Contains(run.stderr, "wrote") {
				t.Fatalf("code %d stderr %q", run.code, run.stderr)
			}
			if run.keysOpened || strings.Contains(run.stdout, enterAltScreenSequence) || len(run.frames) != 0 {
				t.Fatalf("the screen opened:\n%q", run.stdout)
			}
			if tc.same && run.stdout != static {
				t.Errorf("the output changed:\n%s\n---- want:\n%s", run.stdout, static)
			}
		})
	}
}
